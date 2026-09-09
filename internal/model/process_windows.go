//go:build windows

package model

import (
	"errors"
	"os"
	"syscall"
)

// sysProcAttr is a no-op on Windows: there are no process groups, so
// only the direct child can be signaled.
func sysProcAttr() *syscall.SysProcAttr {
	return nil
}

// Kill terminates the direct child process and reaps it, so a
// subsequent Wait returns nil.
func (h *procHandle) Kill() error {
	err := h.cmd.Process.Kill()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return stopErr(h.cmd.Wait())
}
