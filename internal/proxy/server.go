package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"

	"llama-swappy/internal/config"
	"llama-swappy/internal/inspect"
	"llama-swappy/internal/model"
	"llama-swappy/internal/version"
)

// InfoPath is the self-description endpoint: it reports the running
// version, the currently loaded model with its time-to-unload, and the
// metadata of every configured model.
const InfoPath = "/llama-swappy/info"

// ModelsPath is the standard OpenAI model list endpoint; it reports the
// configured models as id/name pairs.
const ModelsPath = "/v1/models"

// Server listens for OpenAI-compatible requests, routes them to the
// matching model (starting or swapping it as needed) and reverse
// proxies the request to that model's llama-server.
type Server struct {
	cfg   *config.Config
	mgr   *model.Manager
	log   *slog.Logger
	infos map[string]inspect.Info
}

// New creates the proxy Server. infos holds the metadata parsed at
// startup from each model's run script; keys whose scripts could not be
// parsed are absent and are reported without those fields.
func New(cfg *config.Config, mgr *model.Manager, log *slog.Logger, infos map[string]inspect.Info) *Server {
	if log == nil {
		log = slog.Default()
	}
	if infos == nil {
		infos = map[string]inspect.Info{}
	}
	return &Server{cfg: cfg, mgr: mgr, log: log, infos: infos}
}

type apiError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *Server) writeError(w http.ResponseWriter, code int, msg string) {
	e := apiError{}
	e.Error.Message = msg
	if code < 500 {
		e.Error.Type = "invalid_request_error"
	} else {
		e.Error.Type = "server_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(e)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Self-description endpoints are served by the proxy itself and
	// never proxied to a model.
	switch r.URL.Path {
	case InfoPath, ModelsPath:
		if r.Method != http.MethodGet {
			s.writeError(w, http.StatusMethodNotAllowed, "use GET")
			return
		}
		if r.URL.Path == InfoPath {
			s.serveInfo(w)
		} else {
			s.serveModels(w)
		}
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		s.writeError(w, http.StatusBadRequest, "invalid request: missing model field")
		return
	}
	if _, ok := s.cfg.Models[req.Model]; !ok {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("unknown model %q", req.Model))
		return
	}
	target, err := s.mgr.EnsureModel(req.Model)
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "model not available: "+err.Error())
		return
	}
	ru, err := url.Parse(target)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "bad proxy URL: "+err.Error())
		return
	}
	sw := &startedWriter{ResponseWriter: w}
	rp := httputil.NewSingleHostReverseProxy(ru)
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		s.log.Warn("upstream error", "model", req.Model, "err", err)
		// If the response was already partially streamed the status code
		// cannot be rewritten; the client just sees a truncated response.
		if sw.started {
			return
		}
		s.writeError(sw, http.StatusBadGateway, "upstream error: "+err.Error())
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.log.Info("proxying request", "model", req.Model, "path", r.URL.Path)
	rp.ServeHTTP(sw, r)
	if sw.started && sw.code < 300 {
		s.mgr.MarkServed(req.Model)
	}
}

// modelInfo is one configured model in the info response. Fields that
// could not be derived (script missing, not text, flag absent) are
// omitted.
type modelInfo struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	ContextSize int      `json:"contextSize,omitempty"`
	MaxTokens   int      `json:"maxTokens,omitempty"`
	Reasoning   *bool    `json:"reasoning,omitempty"`
	Input       []string `json:"input"`
}

// currentModel is the loaded model in the info response.
type currentModel struct {
	Key             string `json:"key"`
	Name            string `json:"name"`
	SecondsToUnload int    `json:"secondsToUnload"`
}

// infoResponse is the payload of GET InfoPath.
type infoResponse struct {
	Version string        `json:"version"`
	Current *currentModel `json:"current"`
	Models  []modelInfo   `json:"models"`
}

// serveInfo answers GET InfoPath with the app version, the loaded model
// (with its remaining idle time) and all configured models.
func (s *Server) serveInfo(w http.ResponseWriter) {
	resp := infoResponse{
		Version: version.Version,
		Models:  make([]modelInfo, 0, len(s.cfg.Models)),
	}
	keys := make([]string, 0, len(s.cfg.Models))
	for k := range s.cfg.Models {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		def := s.cfg.Models[key]
		mi := modelInfo{Key: key, Name: def.Name, Input: []string{"text"}}
		mi.ContextSize = s.infos[key].ContextSize
		mi.Reasoning = s.infos[key].Reasoning
		mi.MaxTokens = inspect.MaxTokens(def.MaxTokens, s.infos[key])
		resp.Models = append(resp.Models, mi)
	}
	if key, remaining, ok := s.mgr.IdleInfo(); ok {
		resp.Current = &currentModel{
			Key:             key,
			Name:            s.cfg.Models[key].Name,
			SecondsToUnload: int(remaining.Seconds()),
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// openAIModel is one entry of the GET ModelsPath list.
type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// serveModels answers GET ModelsPath in the standard OpenAI list format
// so generic clients can discover the configured models.
func (s *Server) serveModels(w http.ResponseWriter) {
	keys := make([]string, 0, len(s.cfg.Models))
	for k := range s.cfg.Models {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	data := make([]openAIModel, 0, len(keys))
	for _, key := range keys {
		data = append(data, openAIModel{ID: key, Object: "model", OwnedBy: "llama-swappy"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Object string        `json:"object"`
		Data   []openAIModel `json:"data"`
	}{Object: "list", Data: data})
}

// startedWriter tracks whether any part of the response has been sent
// (and with which status code), so an error handler can tell whether it
// is still allowed to set the status code and the manager can tell
// whether a request was served.
type startedWriter struct {
	http.ResponseWriter
	started bool
	code    int
}

func (w *startedWriter) WriteHeader(code int) {
	w.started = true
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *startedWriter) Write(b []byte) (int, error) {
	w.started = true
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *startedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
