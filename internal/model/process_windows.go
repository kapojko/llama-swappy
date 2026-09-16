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

// Kill terminates the direct child process and returns the reaper
// result, so a subsequent Wait returns the cached exit error.
func (h *procHandle) Kill() error {
	if err := h.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return stopErr(h.result())
}
