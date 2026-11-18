//go:build linux

package monitor

import (
	"bufio"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Linux system source, reading the same files strata-monitor.sh did:
// /proc/stat for CPU utilization (a delta between samples),
// /proc/meminfo for RAM and swap, the hwmon "nvme" sensors for NVMe
// temperatures and /proc/diskstats for NVMe throughput (deltas of
// the cumulative sector counters). CPU temperature comes from the
// coretemp/k10temp class of hwmon sensors (the package temperature),
// the hottest one wins.

// linuxSys holds the previous counters: both utilization and
// throughput are rates, i.e. deltas between samples.
type linuxSys struct {
	log *slog.Logger

	prevCPU struct {
		total, idle uint64
		has         bool
	}
	prevDisk map[string][2]int64 // nvme disk -> {readBytes, writeBytes}
}

func newSystemSource(log *slog.Logger) sysSource {
	return &linuxSys{log: log, prevDisk: map[string][2]int64{}}
}

func (s *linuxSys) name() string { return "linux-proc" }

func (s *linuxSys) sample() (*CPUSample, *RAMSample) {
	return s.cpu("/proc/stat"), s.ram("/proc/meminfo")
}

// cpu reads the aggregate "cpu " line of /proc/stat and computes the
// utilization since the previous call. The first call only stores
// the baseline.
func (s *linuxSys) cpu(path string) *CPUSample {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var total, idle uint64
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			if found {
				break // the aggregate line is the first one
			}
			continue
		}
		found = true
		for i, field := range strings.Fields(line)[1:] {
			v, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				continue
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		break
	}
	if !found {
		return nil
	}
	defer func() { s.prevCPU.total, s.prevCPU.idle, s.prevCPU.has = total, idle, true }()
	if !s.prevCPU.has {
		return nil
	}
	dTotal, dIdle := total-s.prevCPU.total, idle-s.prevCPU.idle
	if dTotal == 0 {
		return nil
	}
	return &CPUSample{
		UtilPct: f64ptr(100 * float64(dTotal-dIdle) / float64(dTotal)),
		TempC:   cpuTempC("/sys/class/hwmon"),
	}
}

// cpuTempC returns the hottest CPU package temperature from the
// coretemp/k10temp/zenpower hwmon sensors, or nil when the platform
// does not expose one.
func cpuTempC(hwmonDir string) *float64 {
	best := -1.0
	for _, hw := range hwmonDirs(hwmonDir) {
		name := readTrim(filepath.Join(hw, "name"))
		switch name {
		case "coretemp", "k10temp", "zenpower", "cpu_thermal":
		default:
			continue
		}
		if v, err := readMilli(filepath.Join(hw, "temp1_input")); err == nil && v > best {
			best = v
		}
	}
	if best < 0 {
		return nil
	}
	return f64ptr(best)
}

// ram reads /proc/meminfo. MemAvailable (not MemFree) is what the
// kernel considers usable, so "used" is Total - Available.
func (s *linuxSys) ram(path string) *RAMSample {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var total, avail, swapTotal, swapFree *uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64) // value is in kB
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = u64ptr(v / 1024)
		case "MemAvailable:":
			avail = u64ptr(v / 1024)
		case "SwapTotal:":
			swapTotal = u64ptr(v / 1024)
		case "SwapFree:":
			swapFree = u64ptr(v / 1024)
		}
	}
	if total == nil {
		return nil
	}
	out := &RAMSample{TotalMiB: total, SwapTotalMiB: swapTotal}
	if avail != nil {
		out.UsedMiB = u64ptr(*total - *avail)
	}
	if swapTotal != nil && swapFree != nil {
		out.SwapUsedMiB = u64ptr(*swapTotal - *swapFree)
	}
	return out
}

// nvmeDiskRe matches whole NVMe namespace disks in /proc/diskstats
// ("nvme0n1"), excluding partitions (nvme0n1p2) and the controller
// alias (nvme0c0n1).
var nvmeDiskRe = regexp.MustCompile(`^nvme\d+n\d+$`)

// linuxNVMe is the Linux NVMe source.
type linuxNVMe struct {
	log *slog.Logger
	sys *linuxSys
}

func newNVMeSource(log *slog.Logger) nvmeSource {
	return &linuxNVMe{log: log, sys: &linuxSys{log: log, prevDisk: map[string][2]int64{}}}
}

func (n *linuxNVMe) name() string { return "linux-nvme" }

// sample pairs each NVMe disk's throughput (from /proc/diskstats)
// with its temperature (from the hwmon "nvme" sensor of the same
// controller) when one exists.
func (n *linuxNVMe) sample() []NVMESample {
	counters, err := readDiskstats("/proc/diskstats")
	if err != nil {
		return nil
	}
	temps := nvmeTemps("/sys/class/hwmon")
	var disks []string
	for name := range counters {
		if nvmeDiskRe.MatchString(name) {
			disks = append(disks, name)
		}
	}
	sort.Strings(disks)
	out := make([]NVMESample, 0, len(disks))
	for _, disk := range disks {
		s := NVMESample{Name: disk}
		if i := strings.LastIndex(disk, "n"); i > 0 {
			if t, ok := temps[nvmeController(disk)]; ok {
				s.TempC = f64ptr(t)
			}
		}
		cur := counters[disk]
		if prev, has := n.sys.prevDisk[disk]; has {
			s.ReadBps = f64ptr(float64(cur[0] - prev[0]))
			s.WriteBps = f64ptr(float64(cur[1] - prev[1]))
		}
		n.sys.prevDisk[disk] = cur
		out = append(out, s)
	}
	return out
}

// readDiskstats parses /proc/diskstats into per-device cumulative
// {readBytes, writeBytes} (sectors are 512 bytes).
func readDiskstats(path string) (map[string][2]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string][2]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// major minor name reads readsMerged sectorsRead msRead
		// writes writesMerged sectorsWritten msWrite ...
		if len(fields) < 10 {
			continue
		}
		r, err1 := strconv.ParseInt(fields[5], 10, 64)
		w, err2 := strconv.ParseInt(fields[9], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out[fields[2]] = [2]int64{r * 512, w * 512}
	}
	return out, sc.Err()
}

// nvmeTemps maps the controller name ("nvme0") to its composite
// temperature from the hwmon nvme sensors.
func nvmeTemps(hwmonDir string) map[string]float64 {
	out := map[string]float64{}
	for _, hw := range hwmonDirs(hwmonDir) {
		if readTrim(filepath.Join(hw, "name")) != "nvme" {
			continue
		}
		// The controller is the device the hwmon hangs off:
		// /sys/class/hwmon/hwmonX -> .../nvme0/nvme0/nvme0 or similar.
		dev, err := filepath.EvalSymlinks(filepath.Join(hw, "device"))
		if err != nil {
			continue
		}
		base := filepath.Base(dev)
		if !strings.HasPrefix(base, "nvme") {
			continue
		}
		if t, err := readMilli(filepath.Join(hw, "temp1_input")); err == nil {
			out[base] = t
		}
	}
	return out
}

// nvmeController maps a namespace disk ("nvme0n1") to its
// controller ("nvme0") — the hwmon device of the controller carries
// the composite temperature.
func nvmeController(disk string) string {
	if i := strings.LastIndex(disk, "n"); i > 0 {
		return disk[:i]
	}
	return disk
}

// hwmonDirs lists the hwmon directories, sorted.
func hwmonDirs(hwmonDir string) []string {
	dirs, err := filepath.Glob(filepath.Join(hwmonDir, "hwmon*"))
	if err != nil {
		return nil
	}
	sort.Strings(dirs)
	return dirs
}
