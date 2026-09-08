package model

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// waitDelay bounds how long Wait waits for the child's I/O pipes to
// close after the process exits. Without it, a grandchild process
// (e.g. a server spawned by a shell script) that keeps the pipes open
// would make Wait block forever.
const waitDelay = 15 * time.Second

// ProcessStarter starts a model by running cmd as a shell-less command
// line. The line is split on whitespace with double quotes honored, so
// `cmd /c "set PORT=%PORT% & llama-server ..."` yields the arguments
// ["cmd", "/c", "set PORT=%PORT% & llama-server ..."]; on Windows exec
// re-quotes each argument for CreateProcess, restoring the original
// command line.
type ProcessStarter struct{}

func (ProcessStarter) Start(cmd string, out io.Writer) (Handle, error) {
	cmd = strings.TrimSpace(cmd)
	args, err := splitCommandLine(cmd)
	if err != nil {
		return nil, err
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("empty cmd")
	}
	c := exec.Command(args[0], args[1:]...)
	c.Stdout = out
	c.Stderr = out
	c.WaitDelay = waitDelay
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &procHandle{cmd: c}, nil
}

// splitCommandLine splits a command line on whitespace, treating double
// quoted segments as a single argument (quotes stripped).
func splitCommandLine(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inQuote := false
	inArg := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			inArg = true
		case (r == ' ' || r == '\t') && !inQuote:
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unbalanced quote in cmd")
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}

type procHandle struct {
	cmd *exec.Cmd
}

func (h *procHandle) Kill() error {
	return h.cmd.Process.Kill()
}

func (h *procHandle) Wait() error {
	return h.cmd.Wait()
}
