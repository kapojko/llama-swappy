//go:build !linux

package poweroff

// safeToPowerOff has no platform checks outside Linux (the sshd/screen/
// tmux session check is a /proc scan): an explicitly configured
// poweroff command on other platforms is trusted as-is.
func safeToPowerOff() (bool, string) { return true, "" }
