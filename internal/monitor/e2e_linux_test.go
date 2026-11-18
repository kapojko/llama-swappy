//go:build linux

package monitor

import (
	"os"
	"testing"
	"time"
)

// E2E tests against real hardware (WSL included). They are skipped
// unless LLAMA_SWAPPY_E2E=1 is set:
//
//	LLAMA_SWAPPY_E2E=1 go test ./internal/monitor -run E2E -v
func e2eEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("LLAMA_SWAPPY_E2E") == "" {
		t.Skip("e2e: set LLAMA_SWAPPY_E2E=1 to run hardware tests")
	}
}

func TestE2ELinuxSys(t *testing.T) {
	e2eEnabled(t)
	s := newSystemSource(testLogger()).(*linuxSys)
	s.sample() // seed the CPU baseline
	time.Sleep(200 * time.Millisecond)
	cpu, ram := s.sample()
	if cpu == nil || cpu.UtilPct == nil {
		t.Fatal("CPU utilization missing after baseline")
	}
	t.Logf("CPU util = %.1f%% temp=%v", *cpu.UtilPct, deref(cpu.TempC))
	if ram == nil || ram.TotalMiB == nil || *ram.TotalMiB == 0 {
		t.Fatal("RAM sample missing")
	}
	t.Logf("RAM = %d/%d MiB swap = %d/%d MiB", deref64(ram.UsedMiB), deref64(ram.TotalMiB),
		deref64(ram.SwapUsedMiB), deref64(ram.SwapTotalMiB))
}

func TestE2ELinuxNVMe(t *testing.T) {
	e2eEnabled(t)
	n := newNVMeSource(testLogger()).(*linuxNVMe)
	n.sample() // seed the rate baselines
	time.Sleep(500 * time.Millisecond)
	devices := n.sample()
	if len(devices) == 0 {
		t.Skip("no NVMe devices visible (WSL hides the host disks; run on bare metal)")
	}
	for _, s := range devices {
		t.Logf("NVMe %s: temp=%vC read=%v B/s write=%v B/s", s.Name,
			deref(s.TempC), deref(s.ReadBps), deref(s.WriteBps))
	}
}

func TestE2EAMDGPUs(t *testing.T) {
	e2eEnabled(t)
	g := newAMDGPUs(testLogger())
	if g == nil {
		t.Skip("no AMD GPUs on this machine (expected in WSL / on NVIDIA boxes)")
	}
	defer g.close()
	samples := g.sample()
	for _, s := range samples {
		t.Logf("GPU %d (%s): temp=%vC junction=%vC mem=%vC util=%v%% vram=%v/%vMiB power=%vW fan=%vrpm sclk=%v/%vMHz mclk=%v/%vMHz",
			s.Index, s.Name, deref(s.TempC), deref(s.JunctionTempC), deref(s.MemTempC),
			deref(s.UtilPct), deref64(s.VRAMUsedMiB), deref64(s.VRAMTotalMiB), deref(s.PowerW),
			derefInt(s.FanRPM), deref32(s.SMClkMHz), deref32(s.SMClkMaxMHz),
			deref32(s.MemClkMHz), deref32(s.MemClkMaxMHz))
		if s.TempC == nil {
			t.Errorf("GPU %d: temperature missing", s.Index)
		}
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
func deref64(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}
func deref32(p *uint32) uint32 {
	if p == nil {
		return 0
	}
	return *p
}
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
