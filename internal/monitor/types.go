// Package monitor implements the optional hardware heartbeat of
// llama-swappy: periodic sampling of GPUs, CPU, RAM and NVMe devices,
// JSONL logging with daily rotation, and the /llama-swappy/monitor
// endpoint payload (current values, last-minute averages, daily and
// weekly maxima).
//
// The design follows the fail-safe doctrine of the strata-monitor
// heartbeat this feature is modeled after: a probe failure never kills
// the sampler — the affected field is simply omitted from that sample.
// Backends are per-platform (NVML on Windows, sysfs on Linux) and are
// only constructed when monitoring is enabled in the config, so a
// disabled monitor performs no hardware access at all.
package monitor

import "time"

// RecordType is the JSON "type" discriminator of JSONL log records.
type RecordType = string

// SampleType marks a periodic sample record in the JSONL log.
const SampleType RecordType = "sample"

// ModelLoadedType and ModelUnloadedType are the record types written
// when the model manager loads or unloads a model, so the hardware
// log also carries the application-side timeline.
const (
	ModelLoadedType   = "model_loaded"
	ModelUnloadedType = "model_unloaded"
)

// GPUSample is one GPU's state at a sample instant. Fields the
// platform or driver does not provide are nil and omitted from JSON.
// AMD (sysfs): TempC is the edge sensor, JunctionTempC and MemTempC
// the junction and memory sensors; FanRPM is the raw fan tachometer.
// NVIDIA (NVML): TempC is the GPU sensor, MemTempC the memory sensor
// when reported, FanPct the fan duty in percent (NVML has no RPM
// query). SMClk*/MemClk* are the current and maximum core/memory
// clocks — the pair is the throttle witness (current far below max
// under 100% load means clock- or power-capping).
type GPUSample struct {
	Index         int      `json:"index"`
	Name          string   `json:"name"`
	TempC         *float64 `json:"tempC,omitempty"`
	JunctionTempC *float64 `json:"junctionTempC,omitempty"`
	MemTempC      *float64 `json:"memTempC,omitempty"`
	UtilPct       *float64 `json:"utilPct,omitempty"`
	VRAMUsedMiB   *uint64  `json:"vramUsedMiB,omitempty"`
	VRAMTotalMiB  *uint64  `json:"vramTotalMiB,omitempty"`
	PowerW        *float64 `json:"powerW,omitempty"`
	FanRPM        *int     `json:"fanRpm,omitempty"`
	FanPct        *int     `json:"fanPct,omitempty"`
	SMClkMHz      *uint32  `json:"smClkMHz,omitempty"`
	SMClkMaxMHz   *uint32  `json:"smClkMaxMHz,omitempty"`
	MemClkMHz     *uint32  `json:"memClkMHz,omitempty"`
	MemClkMaxMHz  *uint32  `json:"memClkMaxMHz,omitempty"`
}

// CPUSample is the CPU state. UtilPct is the overall utilization
// across all cores (not load average, so it is portable). TempC is
// the hottest package sensor where the platform exposes one.
type CPUSample struct {
	UtilPct *float64 `json:"utilPct,omitempty"`
	TempC   *float64 `json:"tempC,omitempty"`
}

// RAMSample is main memory and swap usage.
type RAMSample struct {
	UsedMiB      *uint64 `json:"usedMiB,omitempty"`
	TotalMiB     *uint64 `json:"totalMiB,omitempty"`
	SwapUsedMiB  *uint64 `json:"swapUsedMiB,omitempty"`
	SwapTotalMiB *uint64 `json:"swapTotalMiB,omitempty"`
}

// NVMESample is one NVMe device: its temperature where the platform
// exposes it without privileges, and the read/write throughput since
// the previous sample.
type NVMESample struct {
	Name    string   `json:"name"`
	TempC   *float64 `json:"tempC,omitempty"`
	ReadBps *float64 `json:"readBps,omitempty"`
	WriteBps *float64 `json:"writeBps,omitempty"`
}

// Sample is one full heartbeat: all GPUs, the CPU, RAM, all NVMe
// devices and the model the proxy itself has loaded ("idle" when the
// loader is up with nothing loaded). It is the unit stored in the
// JSONL log and served by the monitor endpoint.
type Sample struct {
	Type  RecordType `json:"type"`
	TS    time.Time  `json:"ts"`
	GPUs  []GPUSample `json:"gpus,omitempty"`
	CPU   *CPUSample `json:"cpu,omitempty"`
	RAM   *RAMSample `json:"ram,omitempty"`
	NVMes []NVMESample `json:"nvmes,omitempty"`
	Model string     `json:"model"`
}

// ModelEvent is an application-side record in the JSONL log: a model
// was loaded or unloaded (swap, idle TTL, crash, shutdown or manual
// unload — the log does not distinguish, the timestamps line up with
// the proxy's own stderr log).
type ModelEvent struct {
	Type  string    `json:"type"` // ModelLoadedType | ModelUnloadedType
	TS    time.Time `json:"ts"`
	Model string    `json:"model"`
}
