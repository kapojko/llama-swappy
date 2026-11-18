//go:build windows

package monitor

import (
	"log/slog"
	"syscall"
	"unsafe"
)

// NVML backend for Windows. nvml.dll is the library NVIDIA ships with
// every driver install (the same interface pynvml wraps in Python and
// the one nvidia-smi is a client of); it is the sanctioned way to
// read NVIDIA GPU state on Windows. It is loaded with NewLazyDLL, so
// the DLL is touched only if this backend is actually constructed —
// on a machine without an NVIDIA driver nothing is loaded, and with
// monitoring disabled in the config this file's init path is never
// reached at all.
//
// The declarations below are the small flat subset of the NVML C API
// the sampler needs. All calls return nvmlReturn_t (0 = success); a
// nonzero result omits that field instead of failing the sample.

var (
	modNVML         = syscall.NewLazyDLL("nvml.dll")
	procInit        = modNVML.NewProc("nvmlInit_v2")
	procShutdown    = modNVML.NewProc("nvmlShutdown")
	procGetCount    = modNVML.NewProc("nvmlDeviceGetCount_v2")
	procGetHandle   = modNVML.NewProc("nvmlDeviceGetHandleByIndex_v2")
	procGetName     = modNVML.NewProc("nvmlDeviceGetName")
	procGetTemp     = modNVML.NewProc("nvmlDeviceGetTemperature")
	procGetUtil     = modNVML.NewProc("nvmlDeviceGetUtilizationRates")
	procGetMemInfo  = modNVML.NewProc("nvmlDeviceGetMemoryInfo")
	procGetMemInfo2 = modNVML.NewProc("nvmlDeviceGetMemoryInfo_v2")
	procGetPower    = modNVML.NewProc("nvmlDeviceGetPowerUsage")
	procGetFanSpeed = modNVML.NewProc("nvmlDeviceGetFanSpeed")
	procGetClock    = modNVML.NewProc("nvmlDeviceGetClockInfo")
	procGetMaxClock = modNVML.NewProc("nvmlDeviceGetMaxClockInfo")
)

// NVML constants (nvml.h): temperature sensors and clock types.
const (
	nvmlTempGPU     = 0 // GPU core temperature
	nvmlTempMemory  = 1 // GPU memory temperature
	nvmlClkGraphics = 0 // core/graphics clock (the AMD equivalent: sclk)
	nvmlClkMem      = 2 // memory clock
	nvmlSuccess     = 0
)

// nvmlUtilization mirrors nvmlUtilization_t (uint32 fields).
type nvmlUtilization struct {
	GPU, Memory, Enc, Dec uint32
}

// nvmlMemoryV1 mirrors nvmlMemory_t: {total, free, used}, where used
// also counts driver-reserved memory.
type nvmlMemoryV1 struct{ Total, Free, Used uint64 }

// nvmlMemoryV2 mirrors nvmlMemory_v2_t, which reports the reserved and
// the allocated amounts separately; its used field is what nvidia-smi
// shows as memory.used. The version field must be initialized with
// NVML_STRUCT_VERSION(Memory, 2) = sizeof | (2 << 24).
type nvmlMemoryV2 struct {
	Version  uint32
	_        uint32 // alignment padding
	Total    uint64
	Reserved uint64
	Free     uint64
	Used     uint64
}

const nvmlMemoryV2Version = 40 | (2 << 24) // sizeof(nvmlMemory_v2_t) | (2 << 24)

// nvmlGPUs is the Windows NVIDIA backend: NVML is initialized once
// when the backend is created and every sample re-queries the device
// handles. Per-call failures are logged once per device and field, so
// a permanently unsupported value (or a wedged driver) cannot flood
// the log at the sampling cadence.
type nvmlGPUs struct {
	log     *slog.Logger
	warned  map[string]struct{}
	devices []nvmlDevice
}

type nvmlDevice struct {
	index  int
	handle uintptr
	name   string
}

// newNVMLGPUs initializes NVML and enumerates the GPU devices. It
// returns nil (no backend) when the driver library is absent or NVML
// cannot initialize — that includes machines without NVIDIA hardware
// at all, which is not an error.
func newNVMLGPUs(log *slog.Logger) *nvmlGPUs {
	if err := modNVML.Load(); err != nil {
		log.Info("monitor: nvml.dll not found; NVIDIA GPUs not monitored")
		return nil
	}
	if r, _, _ := procInit.Call(); r != nvmlSuccess {
		log.Info("monitor: NVML init failed; NVIDIA GPUs not monitored", "nvmlReturn", r)
		return nil
	}
	var count uint32
	if r, _, _ := procGetCount.Call(uintptr(unsafe.Pointer(&count))); r != nvmlSuccess || count == 0 {
		return nil
	}
	g := &nvmlGPUs{log: log, warned: map[string]struct{}{}}
	for i := uint32(0); i < count; i++ {
		var handle uintptr
		if r, _, _ := procGetHandle.Call(uintptr(i), uintptr(unsafe.Pointer(&handle))); r != nvmlSuccess {
			continue
		}
		g.devices = append(g.devices, nvmlDevice{index: int(i), handle: handle, name: g.deviceName(handle)})
	}
	if len(g.devices) == 0 {
		g.close()
		return nil
	}
	log.Info("monitor: NVML backend started", "gpus", len(g.devices))
	return g
}

func (g *nvmlGPUs) name() string  { return "nvml" }
func (g *nvmlGPUs) close() {
	if r, _, _ := procShutdown.Call(); r != nvmlSuccess {
		g.log.Warn("monitor: NVML shutdown failed", "nvmlReturn", r)
	}
}

// deviceName reads the driver's product name once per device.
func (g *nvmlGPUs) deviceName(handle uintptr) string {
	buf := make([]byte, 64)
	if r, _, _ := procGetName.Call(handle, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r != nvmlSuccess {
		return "nvidia"
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i])
		}
	}
	return "nvidia"
}

// nvmlUint calls a proc of the shape nvmlDeviceGetXxx(device, ...,
// uint32*): the device handle is passed first, then any extra
// in-args, then the output pointer.
func (g *nvmlGPUs) nvmlUint(key string, d nvmlDevice, proc *syscall.LazyProc, extra ...uintptr) (uint32, bool) {
	var out uint32
	args := append([]uintptr{d.handle}, extra...)
	args = append(args, uintptr(unsafe.Pointer(&out)))
	if r, _, _ := proc.Call(args...); r != nvmlSuccess {
		g.warnOnce(key+"."+d.name, r)
		return 0, false
	}
	return out, true
}

// warnOnce reports a per-device NVML failure once, so a permanently
// unsupported value (e.g. memory temperature on a card without that
// sensor) costs a single log line, not one per sample.
func (g *nvmlGPUs) warnOnce(key string, r uintptr) {
	if _, seen := g.warned[key]; !seen {
		g.warned[key] = struct{}{}
		g.log.Info("monitor: NVML value unavailable", "field", key, "nvmlReturn", r)
	}
}

func (g *nvmlGPUs) sample() []GPUSample {
	out := make([]GPUSample, 0, len(g.devices))
	for _, d := range g.devices {
		out = append(out, g.sampleDevice(d))
	}
	return out
}

// sampleDevice queries one GPU. Every field is independently
// optional; a card missing sensors just reports fewer fields.
func (g *nvmlGPUs) sampleDevice(d nvmlDevice) GPUSample {
	s := GPUSample{Index: d.index, Name: d.name}

	if t, ok := g.nvmlUint("tempGPU", d, procGetTemp, nvmlTempGPU); ok {
		s.TempC = f64ptr(float64(t))
	}
	if t, ok := g.nvmlUint("tempMemory", d, procGetTemp, nvmlTempMemory); ok {
		s.MemTempC = f64ptr(float64(t))
	}
	if u, ok := g.nvmlUint("power", d, procGetPower); ok {
		s.PowerW = f64ptr(float64(u) / 1000) // milliwatts
	}
	if f, ok := g.nvmlUint("fanSpeed", d, procGetFanSpeed); ok {
		i := int(f)
		s.FanPct = &i // NVML reports fan duty in percent, not RPM
	}
	if c, ok := g.nvmlUint("clockSM", d, procGetClock, nvmlClkGraphics); ok {
		s.SMClkMHz = u32ptr(c)
	}
	if c, ok := g.nvmlUint("clockSMMax", d, procGetMaxClock, nvmlClkGraphics); ok {
		s.SMClkMaxMHz = u32ptr(c)
	}
	if c, ok := g.nvmlUint("clockMem", d, procGetClock, nvmlClkMem); ok {
		s.MemClkMHz = u32ptr(c)
	}
	if c, ok := g.nvmlUint("clockMemMax", d, procGetMaxClock, nvmlClkMem); ok {
		s.MemClkMaxMHz = u32ptr(c)
	}

	var util nvmlUtilization
	if r, _, _ := procGetUtil.Call(d.handle, uintptr(unsafe.Pointer(&util))); r == nvmlSuccess {
		s.UtilPct = f64ptr(float64(util.GPU))
	} else {
		g.warnOnce("utilization."+d.name, r)
	}

	var mem nvmlMemoryV2
	mem.Version = nvmlMemoryV2Version
	if r, _, _ := procGetMemInfo2.Call(d.handle, uintptr(unsafe.Pointer(&mem))); r == nvmlSuccess {
		s.VRAMUsedMiB = u64ptr(mem.Used / (1 << 20))
		s.VRAMTotalMiB = u64ptr(mem.Total / (1 << 20))
	} else {
		// Older drivers without the v2 call: v1's used also counts the
		// driver-reserved memory, so it reads slightly higher than
		// nvidia-smi's number — the closest available value.
		var m1 nvmlMemoryV1
		if r, _, _ := procGetMemInfo.Call(d.handle, uintptr(unsafe.Pointer(&m1))); r == nvmlSuccess {
			s.VRAMUsedMiB = u64ptr(m1.Used / (1 << 20))
			s.VRAMTotalMiB = u64ptr(m1.Total / (1 << 20))
		} else {
			g.warnOnce("memory."+d.name, r)
		}
	}
	return s
}
