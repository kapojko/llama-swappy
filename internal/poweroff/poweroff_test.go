package poweroff

import (
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"llama-swappy/internal/config"
)

// fakeChecker returns a fixed safety verdict.
type fakeChecker struct {
	safe   bool
	detail string
}

func (f fakeChecker) Check() (bool, string) { return f.safe, f.detail }

// fakeExec records calls and returns a fixed error.
type fakeExec struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (f *fakeExec) run(_ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestPoweroff builds a counter with an explicit idle period and
// faked checker/exec (no /proc access, no real command).
func newTestPoweroff(idle time.Duration, checker Checker, execFn func(string) error) *Poweroff {
	return &Poweroff{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		idle:    idle,
		command: "fake-poweroff",
		checker: checker,
		exec:    execFn,
		state:   StateWaiting,
	}
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestNewDisabled(t *testing.T) {
	for _, pc := range []*config.PoweroffConfig{nil, {Enabled: false}, {}} {
		po, err := New(pc, nil)
		if po != nil || err != nil {
			t.Errorf("New(%+v) = (%v, %v), want (nil, nil)", pc, po, err)
		}
	}
}

func TestNewRequiresIdleSeconds(t *testing.T) {
	if _, err := New(&config.PoweroffConfig{Enabled: true}, nil); err == nil {
		t.Fatal("expected error for missing idleSeconds (there is no default)")
	}
}

func TestNewCommandDefault(t *testing.T) {
	if runtime.GOOS != "linux" {
		// Off-Linux there is no default command: an explicit one is
		// required.
		if _, err := New(&config.PoweroffConfig{Enabled: true, IdleSeconds: 100}, nil); err == nil {
			t.Fatal("expected error for missing command on non-Linux")
		}
		po, err := New(&config.PoweroffConfig{Enabled: true, IdleSeconds: 100, Command: "true"}, nil)
		if err != nil || po.Command() != "true" {
			t.Fatalf("New with explicit command = (%v, %v), want ok", po, err)
		}
		return
	}
	po, err := New(&config.PoweroffConfig{Enabled: true, IdleSeconds: 100}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if po.Command() != DefaultCommand {
		t.Errorf("default command = %q, want %q", po.Command(), DefaultCommand)
	}
}

// A counter that never saw a model load never counts.
func TestNoCountingWithoutLoad(t *testing.T) {
	p := newTestPoweroff(time.Minute, fakeChecker{safe: true}, nil)
	p.OnUnloaded() // defensive: the manager cannot emit this without a load
	st := p.Status()
	if st.State != StateWaiting {
		t.Errorf("state = %q, want %q (no model was ever loaded)", st.State, StateWaiting)
	}
	p.OnLoaded()
	if st := p.Status(); st.State != StateWaiting {
		t.Errorf("state after load = %q, want %q", st.State, StateWaiting)
	}
}

func TestCountingAfterLoadUnload(t *testing.T) {
	const idle = time.Minute
	p := newTestPoweroff(idle, fakeChecker{safe: true}, nil)
	defer p.Close()
	p.OnLoaded()
	p.OnUnloaded()
	st := p.Status()
	if st.State != StateCounting {
		t.Fatalf("state = %q, want %q", st.State, StateCounting)
	}
	if st.SecondsLeft <= 0 || st.SecondsLeft > int(idle.Seconds()) {
		t.Errorf("secondsLeft = %d, want in (0, %d]", st.SecondsLeft, int(idle.Seconds()))
	}
	if !st.Enabled || st.IdleSeconds != int(idle.Seconds()) {
		t.Errorf("status = %+v, want enabled with idleSeconds %d", st, int(idle.Seconds()))
	}
}

// A new model load cancels an in-flight countdown.
func TestLoadCancelsCountdown(t *testing.T) {
	p := newTestPoweroff(time.Hour, fakeChecker{safe: true}, nil)
	defer p.Close()
	p.OnLoaded()
	p.OnUnloaded()
	if st := p.Status(); st.State != StateCounting {
		t.Fatalf("state = %q, want %q", st.State, StateCounting)
	}
	p.OnLoaded()
	st := p.Status()
	if st.State != StateWaiting {
		t.Errorf("state after reload = %q, want %q", st.State, StateWaiting)
	}
	// The next unload starts counting again.
	p.OnUnloaded()
	if st := p.Status(); st.State != StateCounting {
		t.Errorf("state after second unload = %q, want %q", st.State, StateCounting)
	}
}

// An unsafe system restarts the full countdown without executing
// anything.
func TestUnsafeRestartsCountdown(t *testing.T) {
	const idle = 40 * time.Millisecond
	fe := &fakeExec{}
	p := newTestPoweroff(idle, fakeChecker{safe: false, detail: "active session processes: 42:sshd"}, fe.run)
	defer p.Close()
	p.OnLoaded()
	p.OnUnloaded()
	time.Sleep(idle * 3)
	if fe.count() != 0 {
		t.Fatalf("poweroff command ran %d times, want 0 (system not safe to power off)", fe.count())
	}
	st := p.Status()
	if st.State != StateCounting {
		t.Errorf("state = %q, want %q (countdown restarted)", st.State, StateCounting)
	}
	if st.Note != "active session processes: 42:sshd" {
		t.Errorf("note = %q, want the block reason", st.Note)
	}
}

// actionRecorder collects the onAction callback invocations.
type actionRecorder struct {
	mu        sync.Mutex
	initiated bool
	command   string
	failure   string
	n         int
}

func (r *actionRecorder) cb(initiated bool, command, failure string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	r.initiated, r.command, r.failure = initiated, command, failure
}

// A failed poweroff command stops counting permanently and records
// the error.
func TestExecFailureStopsCounting(t *testing.T) {
	const idle = 40 * time.Millisecond
	fe := &fakeExec{err: errors.New("exit status 1: Access denied")}
	p := newTestPoweroff(idle, fakeChecker{safe: true}, fe.run)
	ar := &actionRecorder{}
	p.SetAction(ar.cb)
	defer p.Close()
	p.OnLoaded()
	p.OnUnloaded()
	waitFor(t, time.Second, func() bool { return p.Status().State == StateFailed })
	st := p.Status()
	if fe.count() != 1 {
		t.Errorf("command ran %d times, want 1", fe.count())
	}
	if !strings.Contains(st.Error, "Access denied") {
		t.Errorf("error = %q, want it to contain the command failure", st.Error)
	}
	// A later load+unload cycle must not restart the counter.
	p.OnLoaded()
	p.OnUnloaded()
	time.Sleep(idle * 3)
	if st := p.Status(); st.State != StateFailed {
		t.Errorf("state = %q, want %q (counter stopped permanently)", st.State, StateFailed)
	}
	if fe.count() != 1 {
		t.Errorf("command ran %d times after failure, want 1", fe.count())
	}
	ar.mu.Lock()
	if ar.n != 1 || ar.initiated || ar.failure == "" {
		t.Errorf("action callback = %+v, want one failure notification", ar)
	}
	ar.mu.Unlock()
}

// A successful command leaves the counter in the initiated state.
func TestExecSuccess(t *testing.T) {
	const idle = 40 * time.Millisecond
	fe := &fakeExec{}
	p := newTestPoweroff(idle, fakeChecker{safe: true}, fe.run)
	ar := &actionRecorder{}
	p.SetAction(ar.cb)
	defer p.Close()
	p.OnLoaded()
	p.OnUnloaded()
	waitFor(t, time.Second, func() bool { return p.Status().State == StateInitiated })
	if fe.count() != 1 {
		t.Errorf("command ran %d times, want 1", fe.count())
	}
	waitFor(t, time.Second, func() bool {
		ar.mu.Lock()
		defer ar.mu.Unlock()
		return ar.n == 1
	})
	ar.mu.Lock()
	if !ar.initiated || ar.command != "fake-poweroff" || ar.failure != "" {
		t.Errorf("action callback = %+v, want one success notification", ar)
	}
	ar.mu.Unlock()
}

// Close stops a pending countdown; the command never runs.
func TestCloseStopsCountdown(t *testing.T) {
	const idle = 40 * time.Millisecond
	fe := &fakeExec{}
	p := newTestPoweroff(idle, fakeChecker{safe: true}, fe.run)
	p.OnLoaded()
	p.OnUnloaded()
	p.Close()
	time.Sleep(idle * 3)
	if fe.count() != 0 {
		t.Fatalf("command ran %d times after Close, want 0", fe.count())
	}
	if st := p.Status(); st.State != StateWaiting {
		t.Errorf("state after Close = %q, want %q", st.State, StateWaiting)
	}
}

// runCommand: quoting, success and failure on both platforms.
func TestRunCommand(t *testing.T) {
	ok, fail := `sh -c "exit 0"`, `sh -c "exit 3"`
	if runtime.GOOS == "windows" {
		ok, fail = `cmd /c "exit 0"`, `cmd /c "exit 3"`
	}
	if err := runCommand(ok); err != nil {
		t.Errorf("runCommand(%q) = %v, want nil", ok, err)
	}
	if err := runCommand(fail); err == nil || !strings.Contains(err.Error(), "3") {
		t.Errorf("runCommand(%q) = %v, want an exit-status-3 error", fail, err)
	}
	if err := runCommand("  "); err == nil {
		t.Error("runCommand(blank) = nil, want an error")
	}
	if err := runCommand(`unbalanced "quote`); err == nil {
		t.Error("runCommand(unbalanced quote) = nil, want an error")
	}
}
