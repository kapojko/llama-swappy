# Handle unexpected llama-server exit and auto-restart

## Problem

Nothing observes the spawned `llama-server` process. When it terminates
unexpectedly (e.g. segmentation fault, as in the reported incident at
11:30:58, `Segmentation fault (core dumped)`), `Manager.cur` still points
at the dead handle. `EnsureModel` (manager.go `EnsureModel`) happily
returns the stale proxy URL for a request to the same model, and every
proxy attempt fails with `connection refused` until manual intervention.
`Handle.Wait` is only ever called from the `Kill` paths, so an uninitiated
exit goes undetected.

Goal (from the user): at least detect the unexpected exit and restart the
model after a fixed timeout (10s); cap consecutive restart cycles.

## 1. `internal/model/process.go` — single reaper

- Change `procHandle` to own a single reaper:
  - Add `done chan error` plus a mutex-cached result so the exit error is
    produced by exactly one `exec.Cmd.Wait()` call and can be consumed
    idempotently from multiple goroutines.
  - `Start()`: after `c.Start()`, launch `go func() { h.done <- c.Wait() }()`
    and return the handle.
  - `Wait()`: return the cached result (blocking until exit on first call,
    cached thereafter); do not rely on the `ProcessState != nil` check.
  - A helper (e.g. `result()`) reads `<-h.done` once, caches it under a
    mutex, and is the single place that consumes the reaper.
- `Handle` interface is unchanged (`Kill`, `Wait`).

## 2. platform Kill variants consume the reaper result

- `internal/model/process_unix.go` `procHandle.Kill()`: send SIGTERM to the
  process group, wait on the reaper result with `termGrace`, escalate to
  SIGKILL on timeout, return the reaper result.
- `internal/model/process_windows.go` `procHandle.Kill()`: `Process.Kill()`
  (guarding `os.ErrProcessDone`), return the reaper result.
- Prevents the double-`Wait` race that would otherwise arise once the
  manager watcher also calls `Wait`.

## 3. `internal/model/manager.go` — crash detection + fixed-delay restart

- Extract the model-spawn portion of `EnsureModel` into
  `startLocked(key) (string, error)` (build cmd/proxyURL, `Starter.Start`,
  `waitReady`, set `m.cur`), used by both `EnsureModel` and the restart timer.
- Add to `Options` (tests override; default constant, no new CLI/YAML
  surface):
  - `RestartDelay time.Duration` (default constant `defaultRestartDelay =
    10 * time.Second`).
- Add `active.startedAt time.Time` and `active.served bool`.
- After a model passes readiness and `m.cur` is set in `startLocked`, start
  `go m.watch(a)`.
- New `watch(a *active)`:
  - Block on `a.handle.Wait()`.
  - Take `m.mu`. If `m.cur != a`, the exit was self-initiated or already
    handled → ignore.
  - Else log `ERROR "model exited unexpectedly"` (include exit status, e.g.
    `signal: segmentation fault`), clear `m.cur`, apply crash-loop cap
    (step 4), and if allowed schedule a restart.
- Restart scheduling via `time.AfterFunc(m.restartDelay, ...)`; store the
  timer in a manager field under `m.mu`. The callback takes the lock and
  only starts if `m.ctx` is not done and `m.cur == nil`, then runs
  `startLocked(key)` (log INFO `restarting model after crash`).
- Any successful start (in `startLocked`) cancels/clears a pending restart
  timer, so a client request during the 10s window restarts immediately and
  the timer no-ops. The 10s timer only matters when there is no traffic.
- `stopLocked()` sets `m.cur = nil` *before* `Kill` (still inside the held
  lock) so the watcher ignores self-inflicted exits (crash at the same
  instant as an idle-unload/swap).
- `Close()` cancels the pending timer (callback already guards on `m.ctx`).

## 4. Crash-loop cap (user decision: cap consecutive restarts)

- Manager field `crashLoops int`; constant `maxAutoRestarts = 3`.
- Proxy signals a successful response via a new manager method
  `MarkServed()` which sets `m.cur.served = true` (under the lock).
- In `watch()`: if `a.served` → `crashLoops = 0`; increment `crashLoops`.
  If `crashLoops > maxAutoRestarts`, log a warning that auto-restart is
  suppressed and do not schedule a timer. A subsequent client request still
  restarts the model manually and is never capped.
- `MarkServed` is called when a proxy response completes with a 2xx status.

## 5. `internal/proxy/server.go` — report served responses

- In `startedWriter`, record the status code set by `WriteHeader` (default
  200 when `Write` is called without `WriteHeader`).
- In `ServeHTTP`, after `rp.ServeHTTP`, if `sw.started && sw.code < 300`
  call `s.mgr.MarkServed()`.

## 6. Tests

- `fakeHandle`: add a `done chan error` (created by `fakeStarter.Start`)
  and make `Wait()` idempotent (cached result); tests drive a crash by
  sending an error on the handle's channel.
- `manager_test.go`:
  - Crash → `Current()` clears, then auto-restart brings `Current()` back
    to the same key; `fakeStarter` start count goes 1 → 2.
  - Crash + immediate `EnsureModel` → restarts once; after `RestartDelay`
    elapses there is no third start (timer no-op).
  - Crash + `Close()` before `RestartDelay` → no restart.
  - Crash loop: crash 3× without `MarkServed` → 4th auto-restart is
    suppressed; a manual `EnsureModel` still starts.
  - A served (2xx) crash resets the loop counter.
- `internal/testutil/testutil.go`: extend fake-server source with a
  `-crash-after <duration>` flag (start normally, print FAKE-READY, serve,
  then `os.Exit` non-zero).
- New integration test (real `ProcessStarter`, cross-platform): serve
  `/health`, crash after a short delay, verify auto-restart brings `/health`
  back to 200 with a small `RestartDelay` override.

## 7. Docs (required by AGENTS.md)

- `README.md` "Behavior": add a *Crash recovery* bullet (unexpected exit is
  detected, model cleared, auto-restart after 10s; immediate on next
  request; capped after 3 consecutive un-served crashes).
- `docs/CODE.md` process-model section: describe the reaper, the exit
  watcher, the fixed-delay restart, and the crash-loop cap.

## Out of scope

- Investigating *why* `llama-server` crashes (separate investigation).
- Making `RestartDelay` / cap values configurable in the YAML or via CLI
  (kept as constants/`Options` to preserve the minimal surface).
