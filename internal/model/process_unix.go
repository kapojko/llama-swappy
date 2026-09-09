//go:build !windows

package model

import (
	"syscall"
	"time"
)

// termGrace bounds how long Kill waits for the process group to exit
// after SIGTERM before escalating to SIGKILL.
const termGrace = 10 * time.Second

// sysProcAttr puts the child in its own process group (Pgid 0 -> the
// child's own PID becomes the group ID), so termination can be sent to
// the whole tree: a wrapper script and the server it spawns.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// Kill sends SIGTERM to the model's entire process group and waits up
// to termGrace for it to exit, escalating to SIGKILL on the group if
// anything survives. The process is reaped before returning, so a
// subsequent Wait returns nil.
func (h *procHandle) Kill() error {
	pid := h.cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case err := <-done:
		return stopErr(err)
	case <-time.After(termGrace):
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return stopErr(<-done)
	}
}
