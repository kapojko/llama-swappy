// Package poweroff implements the optional idle poweroff. When
// enabled, after the loaded model is unloaded the application waits for
// a configurable idle period, checks that the system is safe to shut
// down (on Linux: no ssh sessions, screen or tmux instances) and runs
// the poweroff command. A failed safety check restarts the full
// countdown; a failed poweroff command stops counting permanently (until
// the next application start) and reports its error in Status.
//
// The countdown only starts after the first model has been loaded and
// then unloaded: an application that has served no model yet never
// counts toward a poweroff. Any new model load cancels an in-flight
// countdown; the next unload restarts it from the beginning.
package poweroff

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"llama-swappy/internal/config"
)

// DefaultCommand is the poweroff command used on Linux when the config
// does not provide one.
const DefaultCommand = "/usr/bin/systemctl poweroff"

// Status states reported by Status().
const (
	// StateWaiting: enabled, but no model has been loaded and unloaded
	// yet, so counting has not started.
	StateWaiting = "waiting"
	// StateCounting: the idle countdown toward the safety check is
	// running.
	StateCounting = "counting"
	// StateInitiated: the poweroff command ran successfully and the
	// system should be shutting down.
	StateInitiated = "initiated"
	// StateFailed: the poweroff command could not run; counting
	// stopped.
	StateFailed = "failed"
	// stateDisabled is reported by Status for a nil (disabled) counter.
	stateDisabled = "disabled"
)

// Checker reports whether the system is safe to power off (no active
// human sessions). detail explains an unsafe verdict (logged and
// exposed in Status).
type Checker interface {
	Check() (safe bool, detail string)
}

// Status is the poweroff state as served by /llama-swappy/info.
type Status struct {
	Enabled bool
	// IdleSeconds is the configured idle period before the poweroff
	// check runs.
	IdleSeconds int
	// State is one of StateWaiting, StateCounting, StateInitiated,
	// StateFailed, or "disabled" for a disabled counter.
	State string
	// SecondsLeft is time until the poweroff check; meaningful only in
	// StateCounting.
	SecondsLeft int
	// Note explains the last blocked poweroff (when counting after a
	// restart); empty otherwise.
	Note string
	// Error is the poweroff command failure; set only in StateFailed.
	Error string
}

// Poweroff counts idle time toward a system poweroff. It is driven by
// the model manager's lifecycle events and safe for concurrent use; a
// nil *Poweroff is a no-op for all methods.
type Poweroff struct {
	log     *slog.Logger
	idle    time.Duration
	command string
	checker Checker
	exec    func(cmdline string) error
	// onAction, when set, is notified once the poweroff command has
	// been executed (initiated=true) or failed (initiated=false,
	// with the error). It runs asynchronously, after the outcome is
	// recorded (the monitor uses it to log poweroff actions next to
	// its hardware samples).
	onAction func(initiated bool, command, failure string)

	mu       sync.Mutex
	armed    bool // a model has been loaded at least once
	state    string
	deadline time.Time // countdown end; zero when not counting
	note     string
	errMsg   string
	timer    *time.Timer
	closed   bool
}

// New creates the poweroff counter from the config section. It returns
// nil, nil when the section is absent or disabled: no counting, no
// safety checks, no commands are ever performed. When enabled, the
// command resolves as: an explicit `command` is used as-is on any
// platform; otherwise the Linux default is /usr/bin/systemctl poweroff,
// and on other platforms an error is returned (the default command only
// exists on Linux).
func New(pc *config.PoweroffConfig, log *slog.Logger) (*Poweroff, error) {
	if pc == nil || !pc.Enabled {
		return nil, nil
	}
	if log == nil {
		log = slog.Default()
	}
	if pc.IdleSeconds <= 0 {
		return nil, fmt.Errorf("idleSeconds is required and must be > 0, got %d", pc.IdleSeconds)
	}
	command := pc.Command
	if command == "" {
		if runtime.GOOS != "linux" {
			return nil, fmt.Errorf("no default poweroff command on %s, set poweroff.command explicitly", runtime.GOOS)
		}
		command = DefaultCommand
	}
	return &Poweroff{
		log:     log,
		idle:    time.Duration(pc.IdleSeconds) * time.Second,
		command: command,
		checker: safetyChecker{},
		exec:    runCommand,
		state:   StateWaiting,
	}, nil
}

// safetyChecker is the default Checker: the platform safety check.
type safetyChecker struct{}

func (safetyChecker) Check() (bool, string) { return safeToPowerOff() }

// Command returns the resolved poweroff command line.
func (p *Poweroff) Command() string {
	if p == nil {
		return ""
	}
	return p.command
}

// SetAction registers the action callback (see the onAction field).
// It must be called before any model load, not while a countdown is
// running.
func (p *Poweroff) SetAction(fn func(initiated bool, command, failure string)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.onAction = fn
	p.mu.Unlock()
}

// OnLoaded reports that a model became the loaded model. It cancels an
// in-flight countdown (the system is in use again) and keeps the
// counter armed so the next unload starts counting. It is a no-op once
// the poweroff has been initiated or has failed.
func (p *Poweroff) OnLoaded() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == StateInitiated || p.state == StateFailed {
		return
	}
	p.armed = true
	p.note = ""
	p.cancelCountdownLocked()
}

// OnUnloaded reports that the loaded model is gone. After at least one
// model load it (re)starts the full idle countdown, unless the poweroff
// has already been initiated or has failed.
func (p *Poweroff) OnUnloaded() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == StateInitiated || p.state == StateFailed || !p.armed {
		return
	}
	p.note = ""
	p.state = StateCounting
	p.startCountdownLocked()
	p.log.Info("poweroff: idle countdown started", "idle", p.idle)
}

// Status reports the current poweroff state. A nil receiver reports a
// disabled counter.
func (p *Poweroff) Status() Status {
	if p == nil {
		return Status{State: stateDisabled}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Status{Enabled: true, IdleSeconds: int(p.idle.Seconds()), State: p.state, Note: p.note, Error: p.errMsg}
	if p.state == StateCounting {
		rem := time.Until(p.deadline)
		if rem < 0 {
			rem = 0
		}
		st.SecondsLeft = int(rem.Seconds())
	}
	return st
}

// Close stops the countdown; a pending poweroff is never triggered by
// Close.
func (p *Poweroff) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	if p.state == StateCounting {
		p.state = StateWaiting
	}
}

// onDeadline fires when the idle countdown expires: it runs the safety
// check (outside the lock — it may block on /proc reads) and either
// restarts the full countdown (not safe) or executes the poweroff
// command (safe).
func (p *Poweroff) onDeadline() {
	p.mu.Lock()
	if p.closed || p.state != StateCounting {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	safe, detail := p.checker.Check()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.state != StateCounting {
		// The countdown was cancelled (model reloaded) while the check
		// was running.
		return
	}
	if !safe {
		p.log.Warn("poweroff: not safe to power off, restarting countdown", "detail", detail, "idle", p.idle)
		p.note = detail
		p.startCountdownLocked()
		return
	}
	p.note = ""
	p.state = StateInitiated
	p.log.Info("poweroff: safe to power off, running poweroff command", "cmd", p.command)
	go p.execCommand()
}

// execCommand runs the poweroff command asynchronously and records the
// outcome: a failed command stops counting permanently (StateFailed), a
// successful one leaves the counter in StateInitiated (the machine
// should be going down; a test command that does not shut the machine
// down just leaves it there).
func (p *Poweroff) execCommand() {
	err := p.exec(p.command)
	var failure string
	p.mu.Lock()
	if !p.closed {
		if err != nil {
			p.state = StateFailed
			p.errMsg = err.Error()
			failure = err.Error()
			p.log.Error("poweroff: command failed, counting stopped", "cmd", p.command, "err", err)
		} else {
			p.log.Info("poweroff: command executed, system should be shutting down", "cmd", p.command)
		}
	}
	cb, closed := p.onAction, p.closed
	p.mu.Unlock()
	if cb != nil && !closed {
		cb(err == nil, p.command, failure)
	}
}

// startCountdownLocked sets the deadline and arms the timer for the
// full idle period. The caller must hold p.mu.
func (p *Poweroff) startCountdownLocked() {
	p.deadline = time.Now().Add(p.idle)
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(p.idle, p.onDeadline)
}

// cancelCountdownLocked stops a running countdown. The caller must hold
// p.mu.
func (p *Poweroff) cancelCountdownLocked() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	if p.state == StateCounting {
		p.state = StateWaiting
	}
}
