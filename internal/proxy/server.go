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

	"llama-swappy/internal/config"
	"llama-swappy/internal/model"
)

// Server listens for OpenAI-compatible requests, routes them to the
// matching model (starting or swapping it as needed) and reverse
// proxies the request to that model's llama-server.
type Server struct {
	cfg *config.Config
	mgr *model.Manager
	log *slog.Logger
}

// New creates the proxy Server.
func New(cfg *config.Config, mgr *model.Manager, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, mgr: mgr, log: log}
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
