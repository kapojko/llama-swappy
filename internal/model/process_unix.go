//go:build !windows

package model

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

const (
	// termGrace bounds how long Kill waits for the process group to exit
	// after SIGTERM before escalating to SIGKILL.
	termGrace = 10 * time.Second
	// killGrace bounds the wait after SIGKILL, and the follow-up wait for
	// the reaper, so a process the kernel cannot kill (e.g. stuck in a GPU
	// driver call) cannot block the caller forever.
	killGrace = 5 * time.Second
	// groupPoll is how often the process group is probed while waiting.
	groupPoll = 25 * time.Millisecond
)

// sysProcAttr puts the child in its own process group (Pgid 0 -> the
// child's own PID becomes the group ID), so termination can be sent to
// the whole tree: a wrapper script and the server it spawns.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// attach is a no-op on Unix: sysProcAttr already put the child in its
// own process group, which covers the whole tree for Kill.
func (h *procHandle) attach() error { return nil }

// release is a no-op on Unix (the job object is a Windows-only resource,
// see process_windows.go).
func (h *procHandle) release() {}

// groupAlive reports whether any process is still in the process group led
// by pid (EPERM means "there, but not ours" and is therefore alive too).
func groupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// waitGroupGone polls until the process group is empty or the deadline
// passes, and reports whether the group is gone.
func waitGroupGone(pid int, deadline time.Time) bool {
	for {
		if !groupAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(groupPoll)
	}
}

// Kill sends SIGTERM to the model's entire process group and waits for the
// whole group to exit, escalating to SIGKILL on the group after termGrace.
//
// Waiting for the group rather than for the direct child is what covers
// wrapper scripts: a script that spawns the server and exits on SIGTERM
// without waiting for it (i.e. without exec, with the server in the
// foreground) is gone after the first signal while the server it started is
// still running — possibly hung in its own shutdown. Watching the group
// keeps the escalation pointed at whatever still holds the VRAM and the
// port. It returns the reaper result, so a subsequent Wait returns the
// cached exit error.
func (h *procHandle) Kill() error {
	pid := h.cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	if !waitGroupGone(pid, time.Now().Add(termGrace)) {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		waitGroupGone(pid, time.Now().Add(killGrace))
	}
	select {
	case <-h.finished:
		return stopErr(h.result())
	case <-time.After(killGrace):
		return fmt.Errorf("model process %d did not exit", pid)
	}
}
