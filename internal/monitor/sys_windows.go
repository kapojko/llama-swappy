//go:build windows

package monitor

import (
	"log/slog"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

// Windows system source: CPU utilization from the kernel's CPU time
// counters (GetSystemTimes, the same numbers Task Manager's idle
// percentage comes from), RAM from GlobalMemoryStatusEx, and NVMe
// throughput from the disk performance counters (the data perfmon
// shows as "Disk Read Bytes/sec").
//
// CPU temperature is deliberately not attempted: on Windows it needs
// vendor WMI namespaces or an external tool, and the agreed rule is
// "omit where unavailable" rather than adding fragile lookups.
//
// NVMe temperature needs admin privileges or an external tool, so it
// is omitted here as well; the device list and throughput are the
// useful part. Reading the disk counters requires a handle with read
// access to \\.\PhysicalDriveN, which Windows grants only to elevated
// processes: when llama-swappy runs unprivileged the devices are
// still listed (from the DOS device namespace, which needs no
// privileges) but their throughput fields are omitted.

var (
	modKernel32       = syscall.NewLazyDLL("kernel32.dll")
	procGetSysTimes   = modKernel32.NewProc("GetSystemTimes")
	procGlobalMem     = modKernel32.NewProc("GlobalMemoryStatusEx")
	procQueryDosDev   = modKernel32.NewProc("QueryDosDeviceW")
)

// memoryStatusEx mirrors MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length                                              uint32
	MemoryLoad                                          uint32
	TotalPhys, AvailPhys                                uint64
	TotalPageFile, AvailPageFile                        uint64
	TotalVirtual, AvailVirtual, AvailExtendedVirtual    uint64
}

// windowsSys holds the previous CPU time snapshot (utilization is a
// delta between samples).
type windowsSys struct {
	log      *slog.Logger
	warnedIO bool

	prevIdle, prevKernel, prevUser uint64
	hasCPU                        bool
}

func newSystemSource(log *slog.Logger) sysSource {
	return &windowsSys{log: log}
}

func (s *windowsSys) name() string { return "windows-sys" }

func (s *windowsSys) sample() (*CPUSample, *RAMSample) {
	return s.cpu(), s.ram()
}

// ftToU64 converts a FILETIME (100ns ticks since 1601) to a uint64.
// Only deltas between two values are ever used, so the epoch offset
// is irrelevant.
func ftToU64(ft syscall.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// cpu computes overall utilization from GetSystemTimes deltas. The
// kernel time includes idle time, so busy = (kernel-idle) + user.
func (s *windowsSys) cpu() *CPUSample {
	var idle, kernel, user syscall.Filetime
	r, _, _ := procGetSysTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return nil
	}
	i, k, u := ftToU64(idle), ftToU64(kernel), ftToU64(user)
	defer func() { s.prevIdle, s.prevKernel, s.prevUser, s.hasCPU = i, k, u, true }()
	if !s.hasCPU {
		return nil // first sample only stores the baseline
	}
	dIdle, dKernel, dUser := i-s.prevIdle, k-s.prevKernel, u-s.prevUser
	dBusy := dKernel - dIdle + dUser
	dTotal := dKernel + dUser
	if dTotal == 0 {
		return nil
	}
	return &CPUSample{UtilPct: f64ptr(100 * float64(dBusy) / float64(dTotal))}
}

// ram reports physical memory and the page file (swap). The page
// file totals are what the OS commits, the closest Windows
// equivalent of Linux swap.
func (s *windowsSys) ram() *RAMSample {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	r, _, _ := procGlobalMem.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 {
		return nil
	}
	return &RAMSample{
		UsedMiB:      u64ptr((ms.TotalPhys - ms.AvailPhys) / (1 << 20)),
		TotalMiB:     u64ptr(ms.TotalPhys / (1 << 20)),
		SwapUsedMiB:  u64ptr((ms.TotalPageFile - ms.AvailPageFile) / (1 << 20)),
		SwapTotalMiB: u64ptr(ms.TotalPageFile / (1 << 20)),
	}
}

// diskPerformance mirrors DISK_PERFORMANCE (ntdddisk.h).
type diskPerformance struct {
	BytesRead, BytesWritten                       int64
	ReadTime, WriteTime, IdleTime                 int64
	ReadCount, WriteCount                         uint32
	QueueDepth, SplitCount                        uint32
	QueryTime                                    int64
	StorageDeviceNumber                          uint32
	StorageManagerName                            [16]uint16
}

// ioctlDiskPerformance is CTL_CODE(IOCTL_DISK_BASE=7, function 8,
// METHOD_BUFFERED, FILE_ANY_ACCESS).
const ioctlDiskPerformance = (7 << 16) | (8 << 2)

// nvmeWindows is the Windows NVMe source. It keeps its own previous
// per-drive IO counters (throughput is a delta), independent of the
// CPU/RAM source.
type nvmeWindows struct {
	log      *slog.Logger
	warned   bool
	prev     map[string][2]int64 // PhysicalDriveN -> {read, written}
}

func newNVMeSource(log *slog.Logger) nvmeSource {
	return &nvmeWindows{log: log, prev: map[string][2]int64{}}
}

func (n *nvmeWindows) name() string { return "windows-nvme" }

func (n *nvmeWindows) sample() []NVMESample {
	names := n.physicalDrives()
	out := make([]NVMESample, 0, len(names))
	for _, name := range names {
		out = append(out, n.sampleDrive(name))
	}
	return out
}

// sampleDrive reports one drive's read/write throughput. The counters
// are cumulative since boot, so the rate is the delta against the
// previous sample. Opening the drive for read access needs elevation;
// without it the drive is still listed, with the fields omitted.
func (n *nvmeWindows) sampleDrive(name string) NVMESample {
	s := NVMESample{Name: name}
	h, err := syscall.Open(`\\.\`+name, syscall.O_RDONLY, 0)
	if err != nil {
		n.warnDriveOnce(name, err)
		return s
	}
	defer syscall.Close(h)
	var perf diskPerformance
	var returned uint32
	err = syscall.DeviceIoControl(h, ioctlDiskPerformance, nil, 0,
		(*byte)(unsafe.Pointer(&perf)), uint32(unsafe.Sizeof(perf)), &returned, nil)
	if err != nil {
		n.warnDriveOnce(name, err)
		return s
	}
	prev, has := n.prev[name]
	n.prev[name] = [2]int64{perf.BytesRead, perf.BytesWritten}
	if !has {
		return s // first sample only stores the baseline
	}
	s.ReadBps = f64ptr(float64(perf.BytesRead - prev[0]))
	s.WriteBps = f64ptr(float64(perf.BytesWritten - prev[1]))
	return s
}

// warnDriveOnce reports a per-drive open failure once: unprivileged
// runs would otherwise repeat it every sample.
func (n *nvmeWindows) warnDriveOnce(name string, err error) {
	if n.warned {
		return
	}
	n.warned = true
	n.log.Info("monitor: disk IO unavailable (unprivileged?)", "drive", name, "err", err)
}

// physicalDrives lists the physical drives from the DOS device
// namespace. QueryDosDeviceW with a null name returns every device
// name in one NUL-separated buffer, ending with a double NUL.
func (n *nvmeWindows) physicalDrives() []string {
	buf := make([]uint16, 64*1024)
	r, _, _ := procQueryDosDev.Call(0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return nil
	}
	cnt := int(r)
	var names []string
	start := 0
	for i := 0; i < int(cnt); i++ {
		if buf[i] == 0 {
			name := syscall.UTF16ToString(buf[start:i])
			if strings.HasPrefix(name, "PhysicalDrive") {
				names = append(names, name)
			}
			start = i + 1
		}
	}
	sort.Strings(names)
	return names
}
