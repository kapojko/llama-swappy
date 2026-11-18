package monitor

import "log/slog"

// gpuSource samples all GPUs known to one backend. A backend returns
// zero samples when it finds no GPUs of its kind; it returns samples
// with omitted fields (not an error) when a GPU is present but a
// value cannot be read — the fail-safe direction: a failed probe must
// never kill the sampler or the app.
type gpuSource interface {
	name() string
	sample() []GPUSample
	close()
}

// sysSource samples CPU and RAM. The two are one source because on
// both platforms they share the same cheap OS interface and both are
// always present; a missing value (e.g. CPU temperature on Windows)
// is a nil field.
type sysSource interface {
	name() string
	sample() (*CPUSample, *RAMSample)
}

// nvmeSource samples all NVMe devices. Devices that exist but cannot
// be read (e.g. no privileges on Windows) are reported with omitted
// fields, so the endpoint still shows what is installed.
type nvmeSource interface {
	name() string
	sample() []NVMESample
}

// newGPUSources returns the GPU backends available on this platform.
// The monitor only calls it when monitoring is enabled, so this is
// the single gate for all GPU hardware access.
//
//   - Windows: NVML (nvml.dll, loaded lazily — machines without an
//     NVIDIA driver never touch it)
//   - Linux: AMD amdgpu sysfs (the strata-monitor approach: plain
//     file reads of the SMU metrics table, deliberately not amd-smi)
//   - Linux + NVIDIA: deliberately not implemented yet; the backend
//     interface is the extension point (NVML via purego, or the
//     nvidia-smi subprocess) once real hardware exists to test on.
func newGPUSources(log *slog.Logger) []gpuSource {
	var srcs []gpuSource
	if g := newNVMLGPUs(log); g != nil {
		srcs = append(srcs, g)
	}
	if g := newAMDGPUs(log); g != nil {
		srcs = append(srcs, g)
	}
	return srcs
}

// guarded runs a backend sampling call and converts a panic (a lazy
// DLL or a driver that misbehaves) into a logged failure plus the
// zero value, so the sampler loop cannot be killed by a backend. A
// panic is logged each time: it should never happen, and silence
// would hide it.
func guarded[T any](log *slog.Logger, src string, fn func() T) (res T) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("monitor backend panicked", "backend", src, "panic", r)
		}
	}()
	return fn()
}

// guarded2 is guarded for functions with two results (the CPU/RAM
// system source).
func guarded2[A, B any](log *slog.Logger, src string, fn func() (A, B)) (a A, b B) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("monitor backend panicked", "backend", src, "panic", r)
		}
	}()
	return fn()
}

// Pointer constructors for the optional sample fields.
func f64ptr(v float64) *float64 { return &v }
func u32ptr(v uint32) *uint32   { return &v }
func u64ptr(v uint64) *uint64   { return &v }
func iptr(v int) *int           { return &v }
