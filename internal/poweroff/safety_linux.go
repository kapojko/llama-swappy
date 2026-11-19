//go:build linux

package poweroff

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// forbiddenExact are process names (as reported by /proc/<pid>/comm)
// that mark the system as having an active human session: sshd, screen
// and tmux. Session/server processes also set a title into comm
// ("sshd: <user>", "tmux: server", ...), matched by forbiddenPrefix.
var (
	forbiddenExact  = map[string]bool{"sshd": true, "screen": true, "tmux": true}
	forbiddenPrefix = []string{"sshd:", "tmux:"}
)

// safeToPowerOff scans /proc for processes that indicate an active
// human session: the system is not safe to power off while any sshd,
// screen or tmux process exists, except the system sshd listener
// ("sshd: /usr/sbin/sshd -D [listener] ..."), which runs even when
// no user session is open.
func safeToPowerOff() (bool, string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, fmt.Sprintf("cannot read /proc: %v", err)
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if !isSessionProcess(name) {
			continue
		}
		// A bare "sshd" comm with -D in the command line is the
		// system listener in daemon mode, not a user session.
		if name == "sshd" && hasDaemonFlag(e.Name()) {
			continue
		}
		found = append(found, fmt.Sprintf("%d:%s", pid, name))
	}
	if len(found) == 0 {
		return true, ""
	}
	return false, "active session processes: " + strings.Join(found, ", ")
}

func hasForbiddenPrefix(name string) bool {
	for _, p := range forbiddenPrefix {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// isSessionProcess reports whether a /proc/<pid>/comm value marks an
// active human session. Titled sshd processes that are the system
// listener ("[listener]" in the title) are excluded: they exist
// regardless of user sessions.
func isSessionProcess(name string) bool {
	if forbiddenExact[name] {
		return true
	}
	return hasForbiddenPrefix(name) && !strings.Contains(name, "[listener]")
}

// hasDaemonFlag reports whether /proc/<pid>/cmdline contains the -D
// flag (sshd "do not detach" mode, i.e. the listener).
func hasDaemonFlag(pid string) bool {
	cmdline, err := os.ReadFile("/proc/" + pid + "/cmdline")
	return err == nil && bytes.Contains(cmdline, []byte("-D"))
}
