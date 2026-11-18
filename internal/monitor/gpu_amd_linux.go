//go:build linux

package monitor

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// AMD GPU backend for Linux, ported from strata-monitor.sh. The
// amdgpu driver publishes its whole state through sysfs: the hwmon
// temperatures, fan, power, the utilization percentage, VRAM usage
// and the DPM clock levels are plain file reads. This is the
// deliberate minimum-cost access path: no process is spawned, and on
// SMU13 every value comes out of the same cached metrics table, so a
// full sample costs no more SMU traffic than a single field would.
// amd-smi (the multi-query interface the SMU wedge correlates with)
// is never touched.
//
// Fail-safe rules inherited from strata-monitor:
//   - a card whose canary read (temp1_input) fails has every GPU
//     field omitted for that sample: when the SMU stops answering,
//     every read below would burn one driver mailbox timeout each,
//     per sample, exactly when the box is in trouble;
//   - only AMD cards are watched (vendor 0x1002), so an NVIDIA card
//     in the same box is not misreported as an AMD one.

// amdGPUs is the Linux AMD backend: the cards are discovered once at
// construction (like strata-monitor, which pins its card at startup)
// and every sample re-reads their sysfs files.
type amdGPUs struct {
	log   *slog.Logger
	cards []amdCard
}

type amdCard struct {
	index   int
	device  string // /sys/class/drm/cardN/device
	hwmon   string // .../device/hwmon/hwmonM
	name    string
}

// newAMDGPUs enumerates the DRM cards that expose an AMD hwmon. It
// returns nil when there are none — a machine with only NVIDIA GPUs
// (or no GPU at all) is not an error.
func newAMDGPUs(log *slog.Logger) *amdGPUs {
	const drmDir = "/sys/class/drm"
	matches, _ := filepath.Glob(filepath.Join(drmDir, "card[0-9]*"))
	sort.Strings(matches)
	g := &amdGPUs{log: log}
	for _, cardPath := range matches {
		dev := filepath.Join(cardPath, "device")
		if readTrim(filepath.Join(dev, "vendor")) != "0x1002" {
			continue
		}
		hw := firstHwmonWithCanary(dev)
		if hw == "" {
			continue
		}
		index, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(cardPath), "card"))
		g.cards = append(g.cards, amdCard{
			index:  index,
			device: dev,
			hwmon:  hw,
			name:   amdCardName(dev, index),
		})
	}
	if len(g.cards) == 0 {
		return nil
	}
	log.Info("monitor: AMD sysfs backend started", "gpus", len(g.cards))
	return g
}

func (g *amdGPUs) name() string { return "amd-sysfs" }
func (g *amdGPUs) close()       {}

// firstHwmonWithCanary returns the card's first hwmon directory that
// has a readable canary (temp1_input), or "".
func firstHwmonWithCanary(dev string) string {
	hwmons, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*"))
	sort.Strings(hwmons)
	for _, hw := range hwmons {
		if _, err := os.Stat(filepath.Join(hw, "temp1_input")); err == nil {
			return hw
		}
	}
	return ""
}

// amdCardName prefers the driver's product name (kernel 5.13+);
// older kernels fall back to a "cardN" label with the PCI id.
func amdCardName(dev string, index int) string {
	if n := readTrim(filepath.Join(dev, "product_name")); n != "" {
		return strings.TrimSpace(n)
	}
	pci := readTrim(filepath.Join(dev, "device"))
	if pci != "" {
		return fmt.Sprintf("card%d [%s]", index, strings.TrimPrefix(pci, "0x"))
	}
	return fmt.Sprintf("card%d", index)
}

func (g *amdGPUs) sample() []GPUSample {
	out := make([]GPUSample, 0, len(g.cards))
	for _, c := range g.cards {
		out = append(out, c.sample())
	}
	return out
}

// sample reads one card. The canary read (temp1_input) comes first:
// when the SMU is not answering, the whole card reports only its
// name — that absence is itself evidence, and the sample cadence
// survives.
func (c amdCard) sample() GPUSample {
	s := GPUSample{Index: c.index, Name: c.name}

	edge, err := readMilli(filepath.Join(c.hwmon, "temp1_input"))
	if err != nil {
		return s // canary failed: omit every GPU field
	}
	s.TempC = f64ptr(edge)

	if v, err := readMilli(filepath.Join(c.hwmon, "temp2_input")); err == nil {
		s.JunctionTempC = f64ptr(v)
	}
	if v, err := readMilli(filepath.Join(c.hwmon, "temp3_input")); err == nil {
		s.MemTempC = f64ptr(v)
	}
	if v, err := readInt(filepath.Join(c.hwmon, "fan1_input")); err == nil {
		s.FanRPM = iptr(int(v))
	}
	if v, err := readInt(filepath.Join(c.hwmon, "power1_average")); err == nil {
		s.PowerW = f64ptr(float64(v) / 1e6) // microwatts
	}
	if v, err := readInt(filepath.Join(c.device, "gpu_busy_percent")); err == nil {
		s.UtilPct = f64ptr(float64(v))
	}
	if v, err := readInt(filepath.Join(c.device, "mem_info_vram_used")); err == nil {
		s.VRAMUsedMiB = u64ptr(uint64(v) / (1 << 20))
	}
	if v, err := readInt(filepath.Join(c.device, "mem_info_vram_total")); err == nil {
		s.VRAMTotalMiB = u64ptr(uint64(v) / (1 << 20))
	}
	cur, max := dpmClocks(filepath.Join(c.device, "pp_dpm_sclk"))
	s.SMClkMHz, s.SMClkMaxMHz = u32ptr(uint32(cur)), u32ptr(uint32(max))
	cur, max = dpmClocks(filepath.Join(c.device, "pp_dpm_mclk"))
	s.MemClkMHz, s.MemClkMaxMHz = u32ptr(uint32(cur)), u32ptr(uint32(max))
	return s
}

// dpmClocks parses a pp_dpm_sclk / pp_dpm_mclk table:
//
//	S: 26Mhz *     1: 500Mhz     2: 2254Mhz
//
// The active level is marked with '*', the maximum is the last level.
// Both are the throttle witness: utilization pinned at 100% with the
// current clock far below the maximum means clock- or power-capping.
// An unreadable table yields (0, 0) — omitted fields.
func dpmClocks(path string) (cur, max uint64) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) == 0 {
			continue
		}
		mhz, err := strconv.ParseUint(strings.TrimSuffix(fields[0], "Mhz"), 10, 32)
		if err != nil {
			continue
		}
		max = mhz
		if strings.Contains(line, "*") {
			cur = mhz
		}
	}
	return cur, max
}

// readTrim reads a small sysfs file and trims whitespace; "" on any
// error.
func readTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readMilli reads a millidegree value and converts to °C.
func readMilli(path string) (float64, error) {
	v, err := readInt(path)
	if err != nil {
		return 0, err
	}
	return float64(v) / 1000, nil
}

// readInt reads a small integer sysfs file.
func readInt(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}
