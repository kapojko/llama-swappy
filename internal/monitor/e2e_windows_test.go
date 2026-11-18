//go:build windows

package monitor

import (
	"os"
	"testing"
	"time"
)

// E2E tests against real hardware. They are skipped unless
// LLAMA_SWAPPY_E2E=1 is set, so the normal suite stays hermetic; run
// them on a machine that actually has the hardware:
//
//	$env:LLAMA_SWAPPY_E2E = "1"; go test ./internal/monitor -run E2E -v
func e2eEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("LLAMA_SWAPPY_E2E") == "" {
		t.Skip("e2e: set LLAMA_SWAPPY_E2E=1 to run hardware tests")
	}
}

func TestE2ENVML(t *testing.T) {
	e2eEnabled(t)
	g := newNVMLGPUs(testLogger())
	if g == nil {
		t.Fatal("NVML backend not created: no nvml.dll / no NVIDIA driver?")
	}
	defer g.close()
	samples := g.sample()
	if len(samples) == 0 {
		t.Fatal("no GPUs reported")
	}
	for _, s := range samples {
		t.Logf("GPU %d (%s): temp=%vC memTemp=%vC util=%v%% vram=%v/%vMiB power=%vW fan=%v%% smClk=%v/%vMHz memClk=%v/%vMHz",
			s.Index, s.Name, deref(s.TempC), deref(s.MemTempC), deref(s.UtilPct),
			deref64(s.VRAMUsedMiB), deref64(s.VRAMTotalMiB), deref(s.PowerW),
			derefInt(s.FanPct), deref32(s.SMClkMHz), deref32(s.SMClkMaxMHz),
			deref32(s.MemClkMHz), deref32(s.MemClkMaxMHz))
		if s.TempC == nil {
			t.Errorf("GPU %d: temperature missing", s.Index)
		}
		if s.VRAMTotalMiB == nil || *s.VRAMTotalMiB == 0 {
			t.Errorf("GPU %d: VRAM total missing", s.Index)
		}
	}
}

func TestE2EWindowsSys(t *testing.T) {
	e2eEnabled(t)
	s := newSystemSource(testLogger()).(*windowsSys)
	// Two calls: the first seeds the CPU baseline, the second has data.
	s.sample()
	time.Sleep(200 * time.Millisecond)
	cpu, ram := s.sample()
	if cpu == nil || cpu.UtilPct == nil {
		t.Fatal("CPU utilization missing after baseline")
	}
	t.Logf("CPU util = %.1f%%", *cpu.UtilPct)
	if ram == nil || ram.TotalMiB == nil || *ram.TotalMiB == 0 {
		t.Fatal("RAM sample missing")
	}
	t.Logf("RAM = %d/%d MiB, swap = %d/%d MiB", deref64(ram.UsedMiB), deref64(ram.TotalMiB),
		deref64(ram.SwapUsedMiB), deref64(ram.SwapTotalMiB))
}

func TestE2EWindowsNVMe(t *testing.T) {
	e2eEnabled(t)
	n := newNVMeSource(testLogger()).(*nvmeWindows)
	first := n.sample()
	time.Sleep(500 * time.Millisecond)
	second := n.sample()
	if len(second) == 0 {
		t.Skip("no physical drives listed")
	}
	for _, s := range second {
		t.Logf("NVMe %s: read=%v B/s write=%v B/s", s.Name, deref(s.ReadBps), deref(s.WriteBps))
	}
	// Rate fields appear from the second sample on — unless the
	// process is unprivileged, in which case they are legitimately
	// absent (the device is still listed).
	if first[0].ReadBps != nil && second[0].ReadBps == nil {
		t.Errorf("first sample had rates, second did not")
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
