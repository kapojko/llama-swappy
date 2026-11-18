package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"llama-swappy/internal/config"
	"llama-swappy/internal/model"
)

// defaultInterval is the heartbeat cadence: one JSONL line every 5
// seconds, the same as strata-monitor.
const defaultInterval = 5 * time.Second

// ringWindow is how far back the in-memory ring reaches; the
// "averaged per last minute" of the monitor endpoint is computed over
// it without touching the log files.
const ringWindow = time.Minute

// Monitor is the optional hardware heartbeat of llama-swappy. It is
// created only when the monitor section of the config is enabled — a
// disabled monitor means this type is never constructed and no
// backend, file or DLL is ever touched.
type Monitor struct {
	log      *slog.Logger
	interval time.Duration
	gpus     []gpuSource
	sys      sysSource
	nvme     nvmeSource
	writer   *logWriter // nil when no logDir is configured
	stats    *statsReader

	mu       sync.Mutex
	ring     []Sample
	modelKey string

	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
	started    bool
	writeFails int
}

// New builds the Monitor from the config section: platform backends,
// the optional log writer, and the in-memory state. The model field
// starts as "idle"; the model manager's event callback keeps it
// current (see ModelEvent).
func New(cfg *config.MonitorConfig, log *slog.Logger) (*Monitor, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil
	}
	interval := defaultInterval
	if cfg.Interval != "" {
		d, err := time.ParseDuration(cfg.Interval)
		if err != nil {
			return nil, err
		}
		interval = d
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Monitor{
		log:      log,
		interval: interval,
		gpus:     newGPUSources(log),
		sys:      newSystemSource(log),
		nvme:     newNVMeSource(log),
		modelKey: "idle",
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	if cfg.LogDir != "" {
		m.writer = newLogWriter(cfg.LogDir, cfg.RetentionDays, log)
		m.stats = newStatsReader(log)
	}
	log.Info("monitor: started", "interval", interval, "logDir", cfg.LogDir,
		"gpusBackends", len(m.gpus))
	return m, nil
}

// Start launches the sampling loop. The first sample is taken
// immediately (it also seeds the rate baselines of the platform
// sources). Calling Start more than once is a no-op.
func (m *Monitor) Start() {
	m.startOnce.Do(func() {
		m.started = true
		go m.loop()
	})
}

// Stop terminates the sampling loop, waits for it and releases the
// backends (NVML shutdown included). It is idempotent and safe to
// call before Start (the loop is then simply never run).
func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		m.cancel()
		if m.started {
			select {
			case <-m.done:
			case <-time.After(5 * time.Second):
				m.log.Warn("monitor: sampling loop did not stop in time")
			}
		} else {
			close(m.done)
		}
		for _, g := range m.gpus {
			g.close()
		}
		if m.writer != nil {
			m.writer.close()
		}
	})
}

// loop is the heartbeat: one sample per interval, into the ring and
// (when configured) the day's JSONL file. A write failure is logged
// sparsely (the first and every 100th) — a full disk must not flood
// the log, but must not stay silent either.
func (m *Monitor) loop() {
	defer close(m.done)
	m.sampleOnce()
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.sampleOnce()
		}
	}
}

func (m *Monitor) sampleOnce() {
	s := m.collect()
	if s.TS.IsZero() {
		return
	}
	m.mu.Lock()
	m.ring = append(m.ring, s)
	cutoff := s.TS.Add(-ringWindow)
	if len(m.ring) > 0 && m.ring[0].TS.Before(cutoff) {
		// Drop everything older than the window; keep at least the
		// newest sample so a slow interval still produces an average.
		drop := 0
		for drop < len(m.ring) && m.ring[drop].TS.Before(cutoff) {
			drop++
		}
		if drop == len(m.ring) {
			drop--
		}
		m.ring = m.ring[drop:]
	}
	m.mu.Unlock()
	if m.writer != nil {
		if err := m.writer.write(s); err != nil {
			m.writeFails++
			if m.writeFails == 1 || m.writeFails%100 == 0 {
				m.log.Error("monitor: log write failed", "err", err, "consecutiveFailures", m.writeFails)
			}
		} else {
			m.writeFails = 0
		}
	}
}

// collect gathers one sample from all backends. A panicking or
// failing backend contributes nothing for that sample — the strata
// fail-safe rule: a probe failure must cost one field in one line,
// not the heartbeat.
func (m *Monitor) collect() Sample {
	m.mu.Lock()
	modelKey := m.modelKey
	m.mu.Unlock()
	s := Sample{Type: SampleType, TS: time.Now().UTC(), Model: modelKey}
	for _, g := range m.gpus {
		s.GPUs = append(s.GPUs, guarded(m.log, g.name(), g.sample)...)
	}
	if m.sys != nil {
		cpu, ram := guarded2(m.log, m.sys.name(), m.sys.sample)
		s.CPU, s.RAM = cpu, ram
	}
	if m.nvme != nil {
		s.NVMes = append(s.NVMes, guarded(m.log, m.nvme.name(), m.nvme.sample)...)
	}
	return s
}

// ModelEvent is the callback wired into the model manager (safe on a
// nil receiver, so the manager can always be constructed with it). It
// updates the sample's model field and writes the event into the JSONL
// log, so the hardware timeline and the model timeline share one
// file.
func (m *Monitor) ModelEvent(action, key string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if action == model.EventLoaded {
		m.modelKey = key
	} else {
		m.modelKey = "idle"
	}
	m.mu.Unlock()
	if m.writer == nil {
		return
	}
	typ := ModelUnloadedType
	if action == model.EventLoaded {
		typ = ModelLoadedType
	}
	if err := m.writer.write(ModelEvent{Type: typ, TS: time.Now().UTC(), Model: key}); err != nil {
		m.log.Warn("monitor: model event not logged", "err", err)
	}
}

// Status is the payload of the /llama-swappy/monitor endpoint. Now is
// the newest sample; AvgMinute averages the in-memory ring; MaxDay
// and MaxWeek come from the log files (omitted when logging is not
// configured or the files hold no samples yet).
func (m *Monitor) Status() Status {
	m.mu.Lock()
	ring := make([]Sample, len(m.ring))
	copy(ring, m.ring)
	m.mu.Unlock()
	st := Status{Enabled: true, HistoryAvailable: m.writer != nil}
	if len(ring) > 0 {
		st.Now = &ring[len(ring)-1]
		avg := combine(ring, combineAvg)
		st.AvgMinute = &avg
	}
	if m.writer != nil {
		if ss := m.stats.readSamples(m.writer.todayFile()); len(ss) > 0 {
			v := combine(ss, combineMax)
			st.MaxDay = &v
		}
		if ss := m.stats.readAll(m.writer.recentFiles(7)); len(ss) > 0 {
			v := combine(ss, combineMax)
			st.MaxWeek = &v
		}
	}
	return st
}

// Status is the JSON body served by the monitor endpoint. When
// monitoring is disabled the endpoint answers with the zero value of
// the first two fields only (Enabled=false, Reason set), so
// downstream consumers can tell "new app, monitoring off" from
// "monitoring on, no history yet".
type Status struct {
	Enabled           bool     `json:"enabled"`
	Reason            string   `json:"reason,omitempty"`
	Now               *Sample  `json:"now,omitempty"`
	AvgMinute         *Sample  `json:"avgMinute,omitempty"`
	MaxDay            *Sample  `json:"maxDay,omitempty"`
	MaxWeek           *Sample  `json:"maxWeek,omitempty"`
	HistoryAvailable  bool     `json:"historyAvailable"`
}
