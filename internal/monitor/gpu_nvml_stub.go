//go:build !windows

package monitor

import "log/slog"

// newNVMLGPUs is the Windows NVIDIA backend; on other platforms no
// NVML-backed source exists. (Linux + NVIDIA is deliberately not
// implemented yet; this is the extension point.)
func newNVMLGPUs(log *slog.Logger) gpuSource {
	_ = log
	return nil
}
