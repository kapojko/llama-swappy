package poweroff

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"llama-swappy/internal/model"
)

// commandWaitDelay caps how long the poweroff command may keep its
// output pipes open after exit: a misconfigured command must not hang
// the counter.
const commandWaitDelay = 30 * time.Second

// runCommand executes the poweroff command line, split on whitespace
// with double quotes honored (no shell). The command is expected to
// return promptly: systemctl poweroff initiates the shutdown and exits
// 0 while the system goes down. Combined output is captured and
// included in the error on a non-zero exit.
func runCommand(cmdline string) error {
	args, err := model.SplitCommandLine(cmdline)
	if err != nil {
		return fmt.Errorf("split command %q: %w", cmdline, err)
	}
	if len(args) == 0 {
		return fmt.Errorf("empty poweroff command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.WaitDelay = commandWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", cmdline, err, strings.TrimSpace(string(out)))
	}
	return nil
}
