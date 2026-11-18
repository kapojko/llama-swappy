//go:build !linux

package monitor

import "log/slog"

// newAMDGPUs is the Linux AMD sysfs backend; on other platforms no
// AMD-backed source exists.
func newAMDGPUs(log *slog.Logger) gpuSource {
	_ = log
	return nil
}
