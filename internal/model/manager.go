package model

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"llama-swappy/internal/config"
)

// Starter spawns the process that runs a model.
type Starter interface {
	Start(cmd string, out io.Writer) (Handle, error)
}

// Handle is a running model process.
type Handle interface {
	Kill() error
	Wait() error
}

// Options configures the Manager. Zero values fall back to sensible
// defaults (TTL from config, 5m readiness timeout, 500ms polling).
type Options struct {
	Logger       *slog.Logger
	Out          io.Writer
	Starter      Starter
	TTL          time.Duration
	ReadyTimeout time.Duration
	PollInterval time.Duration
}

// active tracks the currently loaded model. At most one model is
// loaded at a time; a request for another model "swaps" it in.
type active struct {
	key      string
	handle   Handle
	proxyURL string
}

// Manager owns the lifecycle of the single active model: starting,
// readiness probing, idle unloading and stopping.
type Manager struct {
	cfg          *config.Config
	log          *slog.Logger
	out          io.Writer
	starter      Starter
	ttl          time.Duration
	readyTimeout time.Duration
	poll         time.Duration

	mu         sync.Mutex
	cur        *active
	lastActive time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

// New creates a Manager and starts the idle-monitor goroutine.
func New(cfg *config.Config, opts Options) *Manager {
	if cfg == nil {
		panic("model: config is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	starter := opts.Starter
	if starter == nil {
		starter = ProcessStarter{}
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = time.Duration(cfg.GlobalTTL) * time.Second
	}
	ready := opts.ReadyTimeout
	if ready <= 0 {
		ready = 5 * time.Minute
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:          cfg,
		log:          log,
		out:          out,
		starter:      starter,
		ttl:          ttl,
		readyTimeout: ready,
		poll:         poll,
		ctx:          ctx,
		cancel:       cancel,
	}
	go m.monitor()
	return m
}

// EnsureModel makes sure the model key is loaded and returns its proxy
// URL. If a different model is currently loaded it is unloaded first
// ("swap"). Concurrent callers are serialized; later callers wait for
// the first one to finish and then reuse the loaded model.
func (m *Manager) EnsureModel(key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil && m.cur.key == key {
		m.lastActive = time.Now()
		return m.cur.proxyURL, nil
	}
	if m.cur != nil {
		m.log.Info("swapping out model", "model", m.cur.key, "in", key)
		m.stopLocked()
	}
	def, ok := m.cfg.Models[key]
	if !ok {
		return "", fmt.Errorf("model %q not found in config", key)
	}
	proxyURL := def.ProxyURL(m.cfg.StartPort)
	m.log.Info("starting model", "model", key, "cmd", def.Cmd)
	h, err := m.starter.Start(def.Cmd, m.out)
	if err != nil {
		m.log.Error("start failed", "model", key, "err", err)
		return "", fmt.Errorf("start model %q: %w", key, err)
	}
	if err := m.waitReady(proxyURL); err != nil {
		_ = h.Kill()
		_ = h.Wait()
		m.log.Error("model not ready, unloaded", "model", key, "err", err)
		return "", fmt.Errorf("model %q not ready: %w", key, err)
	}
	m.cur = &active{key: key, handle: h, proxyURL: proxyURL}
	m.lastActive = time.Now()
	m.log.Info("model ready", "model", key, "proxy", proxyURL)
	return proxyURL, nil
}

// Current returns the key of the loaded model, or "" if none.
func (m *Manager) Current() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return ""
	}
	return m.cur.key
}

// StopAll unloads the current model, if any.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

// Close stops the idle monitor and unloads any loaded model.
func (m *Manager) Close() {
	m.cancel()
	m.StopAll()
}

// monitor unloads the active model once it has been idle longer than TTL.
func (m *Manager) monitor() {
	t := time.NewTicker(m.poll)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			// Check and stop atomically under the lock so a request that
			// just refreshed lastActive cannot be unloaded mid-request.
			m.mu.Lock()
			if m.cur != nil && time.Since(m.lastActive) >= m.ttl {
				m.log.Info("unloading model: idle TTL exceeded", "model", m.cur.key, "idle", time.Since(m.lastActive))
				m.stopLocked()
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) stopLocked() {
	if m.cur == nil {
		return
	}
	key := m.cur.key
	m.log.Info("stopping model", "model", key)
	if err := m.cur.handle.Kill(); err != nil {
		m.log.Warn("kill model failed", "model", key, "err", err)
	}
	if err := m.cur.handle.Wait(); err != nil {
		m.log.Warn("wait model failed", "model", key, "err", err)
	}
	m.cur = nil
}

// waitReady polls proxyURL + "/health" until it answers 2xx/3xx or the
// readiness timeout expires.
func (m *Manager) waitReady(proxyURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	url := strings.TrimRight(proxyURL, "/") + "/health"
	deadline := time.Now().Add(m.readyTimeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-m.ctx.Done():
			return fmt.Errorf("manager closed")
		case <-time.After(m.poll):
		}
	}
	return fmt.Errorf("timed out waiting for %s to become ready", url)
}
