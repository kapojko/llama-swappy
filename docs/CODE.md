# llama-swappy — high-level design

llama-swappy is a CLI proxy that manages llama.cpp model servers on demand.
It exposes a single OpenAI-compatible HTTP endpoint, starts the requested
model, proxies the request, and unloads the model after a period of
inactivity. Only **one model is loaded at a time**; a request for another
model "swaps" the current one out first.

## Request flow

```
client (OpenAI API)
   │  POST /v1/... {"model": "<key>", ...}
   ▼
proxy.Server            internal/proxy/server.go
   │  1. parse body, read "model" field
   │  2. unknown model  -> 404
   │  3. mgr.EnsureModel(key)
   ▼
model.Manager           internal/model/manager.go
   │  a. if key already loaded and running -> return proxy URL
   │  b. else stop current model, spawn new one
   │  c. poll <proxy>/health until ready (timeout => error, retry next request)
   │  d. record lastActive
   ▼
llama-server (spawned process, stdout/stderr -> app stdout)
   │
   ▼
ReverseProxy streams the response back to the client
```

## Idle unloading

The manager runs a background ticker. When the active model has had no
requests for `globalTTL` seconds, it is killed and unregistered. Any later
request restarts it.

## Process model

- `Starter`/`Handle` interfaces in `internal/model` decouple process
  spawning from the manager, so tests can run a fake starter against an
  `httptest` "model server".
- The production starter (`ProcessStarter`) runs `cmd` as a plain command
  line (no shell) and wires the child's stdout+stderr directly to the
  application's stdout. The application's own logs go to stderr via
  `slog`. The command line is split on whitespace with double quotes
  honored, so `cmd /c "set PORT=%PORT% & server ..."` becomes the
  arguments ["cmd", "/c", "set PORT=%PORT% & server ..."]; on Windows
  exec re-quotes each argument for CreateProcess, restoring the original
  command line.
- Unloading stops the model's whole process group on Unix (SIGTERM,
  escalating to SIGKILL after a 10s grace), which covers wrapper scripts
  that spawn the server; on Windows only the direct child is killed
   (see README limitations). `Wait` is bounded by `WaitDelay` (30s), so a
   process that keeps the stdout pipe open cannot hang the unload. A
   termination exit (signal or non-zero code) is the expected outcome of
   `Kill` and is not logged as a failure; only non-exit errors such as
   `ErrWaitDelay` are.
- The proxy tracks whether a response has already started streaming; an
  upstream failure after the first bytes are sent cannot rewrite the
  status code, so only a log entry is produced in that case.
- **Crash recovery.** Each handle owns a single *reaper*: one goroutine
  calls `exec.Cmd.Wait` and stores the exit error in a buffered channel
  with a mutex-cached result, so `Kill` and `Wait` can both consume the
  exit idempotently without a double-`Wait` race. The platform `Kill`
  variants send their signal(s), then return the reaper result instead of
  waiting themselves.
- The manager starts a `watch` goroutine per active model. It blocks on
  `handle.Wait`; when the process exits and the model is still current,
  the manager logs the unexpected exit (with its exit status), clears the
  active model and schedules a fixed-delay auto-restart via
  `time.AfterFunc` (default 10s, `Options.RestartDelay`). The restart
  callback only starts the model if the manager is still open and no model
  is loaded; any successful start cancels the pending timer, so a client
  request during the restart window restarts immediately and the timer
  no-ops. `stopLocked` clears the active model *before* killing, so
  self-initiated exits (idle unload, swap, shutdown) are ignored by the
  watcher.
- A crash-loop cap prevents hammering a model that dies without ever
   serving a request: each unexpected exit increments `crashLoops`
   (reset when the exit follows a served model, i.e. one that answered a
   2xx, and also by any explicit `EnsureModel` start, so one model's
   unserved-crash streak cannot suppress another model's restarts); once
   `crashLoops` exceeds `maxAutoRestarts` (3) no further auto-restart is
   scheduled, but an explicit request always restarts the model.
- The proxy records the response status code in its `startedWriter`;
  after each proxied request that completed with a 2xx it calls
  `Manager.MarkServed` so the crash-loop counter can be reset.

## Config

YAML with exactly three top-level keys (see `example/llama-swap-config.yaml`):

- `globalTTL` — idle seconds before unload
- `startPort` — single port reused by whichever model is active; `${PORT}`
  in each model's `cmd` and `proxy` URL is substituted with it
- `models` — map of model key -> `{name, cmd, proxy}`

Validation is in `config.Validate`.

## Package layout

- `main.go` — entrypoint
- `cmd/root.go` — cobra CLI: `--config` (required), `--listen`, `--version`
- `cmd/version.go` — version constant (`Version`)
- `internal/config` — YAML load/validate
- `internal/model` — model lifecycle (start, readiness, idle, stop)
- `internal/proxy` — OpenAI listener + reverse proxy
