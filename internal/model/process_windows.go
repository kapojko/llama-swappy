//go:build windows

package model

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// Windows has no process groups, so tree termination is done with a job
// object: the direct child starts suspended, joins a kill-on-close job
// before its first thread ever runs, and Kill terminates the job. Every
// descendant the child spawns (a wrapper script's python server, the
// engine and vision workers it re-execs and spawns) joins the job too,
// so a single TerminateJobObject takes down the whole tree. The
// kill-on-close flag also means the tree dies if llama-swappy itself
// exits for any reason without unloading first.

const (
	_CREATE_SUSPENDED      = 0x00000004
	_TH32CS_SNAPTHREAD     = 0x00000004
	_THREAD_SUSPEND_RESUME = 0x0002

	_JOB_OBJECT_EXTENDED_LIMIT_INFORMATION = 9
	_JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE    = 0x00002000
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procThread32First            = kernel32.NewProc("Thread32First")
	procThread32Next             = kernel32.NewProc("Thread32Next")
	procOpenThread               = kernel32.NewProc("OpenThread")
	procResumeThread             = kernel32.NewProc("ResumeThread")
)

// jobObjectBasicLimitInformation mirrors JOBOBJECT_BASIC_LIMIT_INFORMATION.
type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// ioCounters mirrors IO_COUNTERS.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// jobObjectExtendedLimitInformation mirrors JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// sysProcAttr creates the direct child suspended, so attach can put it
// into the job before the child gets a chance to spawn descendants
// outside it.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: _CREATE_SUSPENDED}
}

// attach puts the freshly started (still suspended) direct child into a
// kill-on-close job object and resumes it. Called from Start before the
// reaper goroutine runs, so no other goroutine touches the handle yet.
func (h *procHandle) attach() error {
	job, _, callErr := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return fmt.Errorf("CreateJobObjectW: %w", callErr)
	}
	h.job = job
	var info jobObjectExtendedLimitInformation
	info.BasicLimitInformation.LimitFlags = _JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	ret, _, callErr := procSetInformationJobObject.Call(job, _JOB_OBJECT_EXTENDED_LIMIT_INFORMATION,
		uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Sizeof(info)))
	if ret == 0 {
		h.release()
		return fmt.Errorf("SetInformationJobObject: %w", callErr)
	}
	const (
		_PROCESS_SET_QUOTA = 0x00000100
		_PROCESS_TERMINATE = 0x00000001
	)
	ph, _, callErr := procOpenProcess.Call(_PROCESS_SET_QUOTA|_PROCESS_TERMINATE, 0, uintptr(h.cmd.Process.Pid))
	if ph == 0 {
		h.release()
		return fmt.Errorf("OpenProcess(%d): %w", h.cmd.Process.Pid, callErr)
	}
	defer syscall.CloseHandle(syscall.Handle(ph))
	ret, _, callErr = procAssignProcessToJobObject.Call(job, ph)
	if ret == 0 {
		h.release()
		return fmt.Errorf("AssignProcessToJobObject: %w", callErr)
	}
	if err := resumeMainThread(h.cmd.Process.Pid); err != nil {
		h.release()
		return err
	}
	return nil
}

// release closes the job handle. After the tree has exited this only
// frees the handle; on an attach failure the kill-on-close flag takes
// the suspended child down with it.
func (h *procHandle) release() {
	if h.job != 0 {
		_ = syscall.CloseHandle(syscall.Handle(h.job))
		h.job = 0
	}
}

// Kill terminates the model's whole process tree through the job object
// and returns the reaper result, so a subsequent Wait returns the cached
// exit error.
func (h *procHandle) Kill() error {
	if h.job != 0 {
		ret, _, callErr := procTerminateJobObject.Call(h.job, 1)
		if ret == 0 {
			// The job call failed (e.g. the job is already empty); fall
			// back to the direct child so a stop is at least attempted.
			kerr := h.cmd.Process.Kill()
			if kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
				return fmt.Errorf("TerminateJobObject: %w (direct-child kill also failed: %v)", callErr, kerr)
			}
			return fmt.Errorf("TerminateJobObject: %w", callErr)
		}
		return stopErr(h.result())
	}
	if err := h.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return stopErr(h.result())
}

// resumeMainThread resumes the suspended main thread of the child.
// os/exec hides the thread handle CreateProcess returned, so the thread
// is found through the tool-help snapshot: a suspended new process has
// exactly one thread, its main one. CreateToolhelp32Snapshot can fail
// transiently for a thread being created or destroyed (documented
// ERROR_NO_MORE_FILES), hence the retries.
func resumeMainThread(pid int) error {
	for try := 0; ; try++ {
		err := resumeMainThreadOnce(pid)
		if err == nil || try >= 2 {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func resumeMainThreadOnce(pid int) error {
	snap, _, callErr := procCreateToolhelp32Snapshot.Call(_TH32CS_SNAPTHREAD, 0)
	if snap == ^uintptr(0) {
		return fmt.Errorf("CreateToolhelp32Snapshot: %w", callErr)
	}
	defer syscall.CloseHandle(syscall.Handle(snap))

	type threadEntry32 struct {
		Size, Usage, ThreadID, OwnerProcessID uint32
		BasePri, DeltaPri                     int32
		Flags                                 uint32
	}
	var te threadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	ret, _, _ := procThread32First.Call(snap, uintptr(unsafe.Pointer(&te)))
	for ret != 0 {
		if te.OwnerProcessID == uint32(pid) {
			thread, _, callErr := procOpenThread.Call(_THREAD_SUSPEND_RESUME, 0, uintptr(te.ThreadID))
			if thread == 0 {
				return fmt.Errorf("OpenThread(%d): %w", te.ThreadID, callErr)
			}
			defer syscall.CloseHandle(syscall.Handle(thread))
			res, _, _ := procResumeThread.Call(thread)
			if res == ^uintptr(0) { // (DWORD)-1
				return errors.New("ResumeThread failed")
			}
			return nil
		}
		te.Size = uint32(unsafe.Sizeof(te))
		ret, _, _ = procThread32Next.Call(snap, uintptr(unsafe.Pointer(&te)))
	}
	return errors.New("model process has no thread: it exited before it could be started")
}
