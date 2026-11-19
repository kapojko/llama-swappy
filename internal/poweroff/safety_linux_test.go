//go:build linux

package poweroff

import "testing"

// TestIsSessionProcess covers the /proc comm matching rules, most
// importantly the exclusion of the system sshd listener, which runs
// with no user session open.
func TestIsSessionProcess(t *testing.T) {
	cases := map[string]bool{
		// active human sessions:
		"sshd: remoteadmin [priv]":                  true,
		"sshd: remoteadmin@pts/0":                   true,
		"sshd: yury@pts/3 (timeout Thu 2026-01-01)": true,
		"tmux: server":                              true,
		"screen":                                    true,
		"tmux":                                      true,
		// system listener / other processes:
		"sshd": true, // bare name; the listener daemon-flag is checked separately in the loop
		"sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups": false,
		"bash":    false,
		"systemd": false,
	}
	for name, want := range cases {
		if got := isSessionProcess(name); got != want {
			t.Errorf("isSessionProcess(%q) = %v, want %v", name, got, want)
		}
	}
}
