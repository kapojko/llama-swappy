package monitor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llama-swappy/internal/config"
	"llama-swappy/internal/model"
)

// fakes for the platform backends; no hardware is touched.

type fakeGPU struct{ out []GPUSample }

func (f *fakeGPU) name() string       { return "fake-gpu" }
func (f *fakeGPU) sample() []GPUSample { return f.out }
func (f *fakeGPU) close()             {}

type fakeSys struct{}

func (f *fakeSys) name() string { return "fake-sys" }
func (f *fakeSys) sample() (*CPUSample, *RAMSample) {
	util := 42.0
	total := uint64(100)
	used := uint64(37)
	return &CPUSample{UtilPct: &util}, &RAMSample{TotalMiB: &total, UsedMiB: &used}
}

type fakeNVMe struct{}

func (f *fakeNVMe) name() string     { return "fake-nvme" }
func (f *fakeNVMe) sample() []NVMESample {
	t := 40.5
	r := 1024.0
	return []NVMESample{{Name: "nvme0n1", TempC: &t, ReadBps: &r}}
}

// newTestMonitor builds a Monitor around the fakes, with the JSONL
// writer pointed at a temp dir.
func newTestMonitor(t *testing.T, interval time.Duration) (*Monitor, string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := &Monitor{
		log:      log,
		interval: interval,
		gpus:     []gpuSource{&fakeGPU{out: []GPUSample{{Index: 0, Name: "fake"}}}},
		sys:      &fakeSys{},
		nvme:     &fakeNVMe{},
		writer:   newLogWriter(dir, 7, log),
		stats:    newStatsReader(log),
		modelKey: "idle",
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	t.Cleanup(m.Stop)
	return m, dir
}

func TestMonitorLoopSamplesWritesAndStatus(t *testing.T) {
	m, dir := newTestMonitor(t, 10*time.Millisecond)
	m.Start()
	defer m.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		n := len(m.ring)
		m.mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d samples after 2s", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	st := m.Status()
	if !st.Enabled || !st.HistoryAvailable {
		t.Fatalf("Status = %+v, want enabled with history", st)
	}
	if st.Now == nil {
		t.Fatal("Status.Now is nil")
	}
	if st.Now.Model != "idle" {
		t.Errorf("Model = %q, want idle", st.Now.Model)
	}
	if st.AvgMinute == nil {
		t.Fatal("Status.AvgMinute is nil")
	}
	if st.MaxDay == nil || st.MaxWeek == nil {
		t.Fatal("Status.MaxDay/MaxWeek is nil despite a written log")
	}
	if got := len(st.Now.GPUs); got != 1 || st.Now.GPUs[0].Name != "fake" {
		t.Errorf("GPUs = %+v, want one fake entry", st.Now.GPUs)
	}
	if st.MaxDay.GPUs[0].Name != "fake" {
		t.Errorf("MaxDay GPUs = %+v, want the fake GPU read back from the log", st.MaxDay.GPUs)
	}

	// The log file exists, holds JSONL lines and only today's file.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("log dir entries = %v, err = %v; want exactly one day file", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 3 {
		t.Fatalf("log has %d lines, want >= 3", len(lines))
	}
	var s Sample
	if err := json.Unmarshal([]byte(lines[0]), &s); err != nil {
		t.Fatalf("first line is not JSON: %v", err)
	}
	if s.Type != SampleType || s.Model != "idle" {
		t.Errorf("first line = %+v, want a sample record for idle", s)
	}
}

func TestMonitorModelEvents(t *testing.T) {
	m, dir := newTestMonitor(t, time.Hour) // no periodic samples needed
	m.ModelEvent(model.EventLoaded, "m1")
	m.ModelEvent(model.EventUnloaded, "m1")

	m.mu.Lock()
	modelKey := m.modelKey
	m.mu.Unlock()
	if modelKey != "idle" {
		t.Errorf("modelKey = %q, want idle after unload", modelKey)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("log dir entries = %v, want one day file", entries)
	}
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("log has %d lines, want 2 events", len(lines))
	}
	var ev ModelEvent
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != ModelLoadedType || ev.Model != "m1" {
		t.Errorf("event = %+v, want model_loaded m1", ev)
	}

	// The loaded state is visible in a collected sample.
	m.ModelEvent(model.EventLoaded, "m2")
	if s := m.collect(); s.Model != "m2" {
		t.Errorf("collect().Model = %q, want m2", s.Model)
	}
}

func TestMonitorNilReceiver(t *testing.T) {
	var m *Monitor
	m.ModelEvent(model.EventLoaded, "x") // must not panic
	m.AppStarted("1.10")
	m.PoweroffAction(true, "cmd", "")
}

func TestMonitorAppStartedAndPoweroffAction(t *testing.T) {
	m, dir := newTestMonitor(t, time.Hour) // no periodic samples needed
	m.AppStarted("1.10")
	m.PoweroffAction(true, "/usr/bin/systemctl poweroff", "")
	m.PoweroffAction(false, "/usr/bin/systemctl poweroff", "exit status 1: Access denied")

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("log dir entries = %v, want one day file", entries)
	}
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("log has %d lines, want 3", len(lines))
	}
	var started AppStarted
	if err := json.Unmarshal([]byte(lines[0]), &started); err != nil {
		t.Fatal(err)
	}
	if started.Type != AppStartedType || started.Version != "1.10" {
		t.Errorf("record = %+v, want app_started 1.10", started)
	}
	var ok PoweroffAction
	if err := json.Unmarshal([]byte(lines[1]), &ok); err != nil {
		t.Fatal(err)
	}
	if ok.Type != PoweroffInitiatedType || ok.Command != "/usr/bin/systemctl poweroff" || ok.Error != "" {
		t.Errorf("record = %+v, want poweroff_initiated without error", ok)
	}
	var failed PoweroffAction
	if err := json.Unmarshal([]byte(lines[2]), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Type != PoweroffFailedType || failed.Error != "exit status 1: Access denied" {
		t.Errorf("record = %+v, want poweroff_failed with the error", failed)
	}
}

func TestMonitorNewDisabled(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := New(nil, log)
	if err != nil || m != nil {
		t.Fatalf("New(nil) = %v, %v; want nil, nil", m, err)
	}
	m, err = New(&config.MonitorConfig{}, log)
	if err != nil || m != nil {
		t.Fatalf("New(disabled) = %v, %v; want nil, nil", m, err)
	}
}

func TestMonitorNewEnabledNoLogDir(t *testing.T) {
	// Enabled without logDir: runs, but no writer and no history.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := New(&config.MonitorConfig{Enabled: true, Interval: "1s"}, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(m.Stop)
	if m.writer != nil {
		t.Error("writer is set without logDir")
	}
	st := m.Status()
	if st.Enabled != true || st.HistoryAvailable {
		t.Errorf("Status = %+v, want enabled without history", st)
	}
	if st.Now != nil || st.AvgMinute != nil {
		t.Error("Status has data before Start")
	}
}
