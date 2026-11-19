# llama-swappy — high-level design

llama-swappy is a CLI proxy that manages llama.cpp model servers on demand.
It exposes a single OpenAI-compatible HTTP endpoint, starts the requested
model, proxies the request, and unloads the model after a period of
inactivity. Only **one model is loaded at a time**; a request for another
model "swaps" the current one out first.

## Request flow

```
client (OpenAI API)
   │  POST /v1/... or /llama/<endpoint>  {"model": "<key>", ...}
   ▼
proxy.Server            internal/proxy/server.go
   │  1. serve /llama-swappy/*, /v1/models itself; strip /llama prefix
   │     and whitelist-check the native endpoint
   │  2. parse body, read "model" field
   │  3. unknown model  -> 404
   │  4. mgr.EnsureModel(key)
   ▼
model.Manager           internal/model/manager.go
   │  a. if key already loaded and running -> return proxy URL
   │  b. else stop current model, spawn new one
   │  c. poll <proxy>/health until ready (process exit => fail fast;
   │     timeout => error; both schedule a backoff auto-retry)
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
request restarts it. The idle time is counted from the last *completed*
successful response (`MarkServed`), not from the request start alone, so a
model that just finished a response is not unloaded immediately after. A
single request that runs longer than the TTL can still be unloaded
mid-stream, since the timer is only refreshed when a response completes.

## Idle poweroff (optional)

The `poweroff` config section (absent or `enabled: false` by default)
starts an idle poweroff counter in `internal/poweroff`. It is driven by
the manager's existing `Options.OnEvent` lifecycle callback, which
`cmd/run` multiplexes to the monitor log and the counter
(`loaded` -> `OnLoaded`, `unloaded` -> `OnUnloaded`):

- **Arming.** The countdown starts only after the first model load and
  its subsequent unload: `OnLoaded` arms the counter (and cancels any
  in-flight countdown — the system is in use again), `OnUnloaded`
  (re)starts the full `idleSeconds` countdown only when armed. An app
  that served no model yet never counts toward a poweroff.
- **Safety check.** One `time.AfterFunc` per countdown; at expiry the
  platform safety check runs (outside the counter's lock). On Linux it
  scans `/proc/*/comm` for `sshd` / `sshd: <user>` / `screen` / `tmux`
  processes; any hit restarts the full countdown and records the reason
  as `note` (visible in `/llama-swappy/info` and the stderr log). Off
  Linux the check always passes (an explicit command there is trusted
  as-is).
- **Execution.** A safe check marks the counter `initiated` and runs
  the command asynchronously (whitespace split with double quotes
  honored via the model package's `SplitCommandLine`, no shell, 30s
  WaitDelay). The command is expected to return promptly (`systemctl
  poweroff` initiates the shutdown and exits 0). A failed run sets the
  state to `failed` with the error and **stops counting permanently**
  (until the next application start); a success leaves it `initiated`.
  Both outcomes are logged to stderr and, through the counter's
  `onAction` callback, written to the monitor JSONL log
  (`poweroff_initiated` / `poweroff_failed`).
- **States.** `waiting` (enabled, not armed yet), `counting`,
  `initiated`, `failed`; `Status()` reports them plus `secondsLeft`
  (counting) and the note/error, for the `/llama-swappy/info` payload.
  A nil `*Poweroff` is a no-op for every method, so wiring is the same
  with the feature off.
- The command resolves as: explicit `poweroff.command` as-is on any
  platform; otherwise the Linux default `/usr/bin/systemctl poweroff`,
  and an enabled section without a command on other platforms is a
  startup error (like a bad config). `idleSeconds` has no default and
  must be > 0 when enabled (config validation).

The normal `go test` suite covers the state machine hermetically
(fake checker/exec, small durations). E2E tests
(`internal/poweroff/e2e_linux_test.go`, Linux only, skipped unless
`LLAMA_SWAPPY_E2E=1`) exercise the real `/proc` scan with a tmux
session started and killed by the test, and the full real scenario with
a reduced 5s timer and the real poweroff command — destructive: the
machine/WSL VM goes down (on WSL2 the Windows host is unaffected).

## Process model

- `Handle` interface in `internal/model` decouples process
  spawning from the manager, so tests can run a fake starter against an
  `httptest` "model server". `Handle` exposes `Kill`, `Wait` and a
  `Done` channel (closed on process exit) used for fail-fast readiness.
- The production starter (`ProcessStarter`) runs `cmd` as a plain command
  line (no shell) and wires the child's stdout+stderr directly to the
  application's stdout. The application's own logs go to stderr via
  `slog`. The command line is split on whitespace with double quotes
  honored, so `cmd /c "set PORT=%PORT% & server ..."` becomes the
  arguments ["cmd", "/c", "set PORT=%PORT% & server ..."]; on Windows
  exec re-quotes each argument for CreateProcess, restoring the original
  command line. The recommended Windows invocation is a PowerShell
  wrapper that takes the port as a parameter:
  `powershell -NoProfile -ExecutionPolicy Bypass -File <script>.ps1 -Port ${PORT}`;
  the script declares `param([int]$Port)` (see README and
  `example/run_qwen3.8-27b.ps1`). On Unix the same shape is a bash script
  that takes the port either from `${PORT}` or as its first positional
  argument, and whose **last line is `exec "$SERVER_BIN" ...`**: `exec`
  replaces the shell, so the server is the process llama-swappy spawned,
  watches and signals. Without it the shell is the direct child and the
  server a grandchild — the shell dies on the first SIGTERM while the
  server keeps running, the crash watcher reports a bogus
  `model exited unexpectedly` for the shell, and the stop has to rely on
  the group escalation described below (see README, "Run scripts on Unix").
- Unloading stops the model's whole process group on Unix: SIGTERM, then a
  poll of the group (`kill(-pgid, 0)`, every 25ms) until it is empty or
  `termGrace` (10s) expires, then SIGKILL on the group and a bounded
  `killGrace` (5s) wait; if the direct child still has not been reaped the
  call returns an error instead of blocking. Watching the group rather than
  the direct child is what covers wrapper scripts whose shell exits before
  the server it started does — and a server that hangs in its own shutdown
  (e.g. llama.cpp's second-interrupt `exit()` racing GPU teardown) is
  consequently bounded by the grace instead of living until systemd's
  `TimeoutStopSec`. On Windows the tree is terminated via a
  kill-on-close job object the child joins while still suspended
  (see README limitations). `Wait` is bounded by `WaitDelay` (30s), so a
    process that keeps the stdout pipe open cannot hang the unload. A
    termination exit (signal or non-zero code) is the expected outcome of
    stopping the model: both the `Kill` result and the `Wait` result in
    `stopLocked` run through `stopErr`, so such exits are not logged as
    failures; only non-exit errors such as `ErrWaitDelay` are.
- The proxy tracks whether a response has already started streaming; an
  upstream failure after the first bytes are sent cannot rewrite the
  status code, so only a log entry is produced in that case.
- **Crash recovery.** Each handle owns a single *reaper*: one goroutine
  calls `exec.Cmd.Wait` and stores the exit error in a buffered channel
  with a mutex-cached result, so `Kill` and `Wait` can both consume the
  exit idempotently without a double-`Wait` race. The platform `Kill`
  variants send their signal(s) themselves (on Unix polling the group until
  it is gone) and then return the reaper result.
- The manager starts a `watch` goroutine per active model. It blocks on
  `handle.Wait`; when the process exits and the model is still current,
  the manager logs the unexpected exit (with its exit status), clears the
  active model and schedules a backoff auto-restart via `time.AfterFunc`
  (see below). The restart callback only starts the model if the manager
  is still open and no model is loaded; any successful start cancels the
  pending timer, so a client request during the restart window restarts
  immediately and the timer no-ops. `stopLocked` clears the active model
  *before* killing, so self-initiated exits (idle unload, swap, shutdown)
  are ignored by the watcher.
- **Failed starts are retried too.** A start can fail in two ways: the
  spawn itself errors, or the process dies (or never answers `/health`)
  during the readiness wait — e.g. a llama-server that cannot allocate
  GPU memory because the driver has not finished releasing the previous
  model. The readiness wait selects on the handle's `Done` channel, so an
  exited process fails the start in ~1s instead of burning the whole 5min
  readiness timeout. Both failure kinds go through `failStartLocked`,
  which schedules the same auto-restart as a crash: the client still gets
  its immediate error (503 via the proxy), but the swap self-heals once
  the transient cause (e.g. a GPU driver slowly releasing memory) is
  gone — without waiting for the client to retry.
- **Backoff.** The auto-restart delay doubles with each consecutive
  failure of the same model (`Options.RestartDelay`, default 10s, then
  20s, 40s, ... capped at `maxBackoff`, 2 minutes), so a model whose
  start keeps failing is retried over minutes instead of being hammered.
- A crash-loop cap prevents hammering a model that dies without ever
   serving a request: each failed start or unexpected exit increments
   `crashLoops` for that model key (reset when it follows a served model,
   i.e. one that answered a 2xx, and by any explicit `EnsureModel` request
   for the key, so one model's failure streak cannot suppress another
   model's restarts); once the count exceeds `maxAutoRestarts` (5) no
   further auto-restart is scheduled, but an explicit request always
   restarts the model.
- The proxy records the response status code in its `startedWriter`;
  after each proxied request that completed with a 2xx it calls
  `Manager.MarkServed(key)` with the model key. `MarkServed` resets the
  crash-loop counter and refreshes `lastActive` (see Idle unloading); it
  is a no-op if key is no longer the active model, e.g. when an in-flight
  response of a swapped-out model completes after the swap.

## Self-description and control endpoints

Besides proxying, the server answers two GET endpoints and one POST
endpoint itself (never proxied to a model; any other method gets 405):

- `GET /llama-swappy/info` — JSON with the app `version`, the currently
  loaded model (`key`, `name`, `secondsToUnload` — remaining idle time
  from the same clock the unload monitor uses; `null` when nothing is
  loaded), the poweroff state (`poweroff` object — always present, so
  clients can tell "disabled" from an older server: `enabled`,
  `idleSeconds`, `status` — `disabled` / `waiting` / `counting` /
  `initiated` / `failed` — `secondsLeft` while counting, `note` with
  the last blocked poweroff reason, `error` when the command
  failed) and every configured model (`key`, `name`, `input` —
  `["text"]`, plus `"image"` when the run script declares an image
  reader — plus derived fields; `contextSize` is
  the per-request context, `slots` the parallel slot count, both
  omitted when unknown). Intended for
  clients such as the pi agent to configure models automatically.
- `GET /v1/models` — the standard OpenAI model list, so generic
  OpenAI clients can discover the configured model keys.
- `POST /llama-swappy/unload` — unloads the currently loaded model
  immediately (the same code path the idle unload uses; the proxy keeps
  running and the next request restarts the model). Answers with the
  key of the stopped model as `unloaded`, or `null` when nothing was
  loaded.
- `GET /llama-swappy/monitor` — the hardware monitoring status (see
  below). Always registered: with monitoring disabled it answers
  `200` with `{"enabled":false,"reason":"monitoring is disabled in
  config"}`, so a consumer can tell "new app, monitoring off" from
  "old app without the endpoint". When enabled it serves `now` (the
  newest sample), `avgMinute` (average of the in-memory ring, last 60s),
  `maxDay` and `maxWeek` (maxima over today's log file / the last seven
  files; omitted when logging is not configured or holds no samples
  yet), and `historyAvailable`.
- `POST /llama/<path>` — forwards select native llama-server endpoints
  (`tokenize`, `detokenize`, `apply-template`) to the target model's
  llama-server with the prefix stripped; the whitelist is the
  `llamaNativeEndpoints` map in `internal/proxy/server.go`, anything
  else under the prefix gets 404. These endpoints are tokenizer and
  template helpers without prefill/decode. The `model` body field still
  routes the request (and loads/swaps the model in as usual). The
  whitelist lives in the proxy so a fronting reverse proxy only needs
  one stable routing rule for the `/llama/` prefix.

**Model metadata** is parsed at startup from each model's run script
(`internal/inspect`): the wrapper script referenced by `cmd` (a token
ending in `.sh`, `.bash`, `.ps1`, `.bat` or `.cmd`, after `${PORT}`
substitution) is read; if there is none, the `cmd` line itself is parsed
as llama-server arguments. Parsing handles Unix and Windows line
endings, `#` comments (leading and trailing), bash/PowerShell quoting,
and takes the last uncommented occurrence when a flag appears several
times. Derived fields: `contextSize` (`-c` / `--ctx-size`, divided by
the parallel slot count — see below), `slots` (`-np` / `--parallel`,
omitted when the flag is absent; llama-server's own default applies —
a single slot on older builds, auto on newer ones),
`reasoning` (`--reasoning on|off`, bare flag = on), and `maxTokens`
(explicit per-model `maxTokens` config > script `--max-tokens` >
`min(32768, contextSize/2)`). `input` is derived from the image reader:
`-mm`/`--mmproj` (llama-server with an mmproj file) or `--vision`
(Strata, also accepting a device name or on/off) add `"image"` to
`["text"]`. Because llama-server divides the total
`-c` value equally among the parallel slots, `contextSize` is the
per-request context (total/slots); for `np=1` models it is unchanged.
(With `--kv-unified` llama-server may let a slot exceed its equal
share; the scripts llama-swappy runs do not use it.) A model whose script cannot be read or
parsed (missing file, binary, ...) still works and is listed, but
without the derived fields; the failure is logged as a warning at
startup. Each model's parsed metadata is also logged at startup.
Every uncommented line is tokenized, so no-op "hint" lines can declare
metadata for runners that hide the real flags in their own config: bash
`:` no-op arguments (`: -c 115000 -np 1`) and a PowerShell stub function
(`llama-swappy-metadata -c 115000 -np 1`, defined as `function
llama-swappy-metadata {}`) are both parsed as plain flags. Hint values
must be literal numbers. A hint line can also declare an image reader
(`--vision` for Strata, `-mm`/`--mmproj` for llama-server) when the
real flag lives in the runner's own config file.

## Hardware monitoring (optional)

The `monitor` config section (absent or `enabled: false` by default —
when disabled, no hardware is accessed at all) starts a heartbeat
sampler in `internal/monitor`: one sample per `interval` (default 5s)
of all GPUs (0-N), CPU, RAM and all NVMe devices. The design follows
the strata-monitor fail-safe doctrine: a probe failure never kills
the sampler — the affected field is omitted for that sample, and a
permanently unavailable value is logged once, not once per sample
(backends also run behind a `recover` guard).

**Backends** (each in its own file, selected per platform at
construction — `newGPUSources`/`newSystemSource`/`newNVMeSource`):

- `gpu_nvml_windows.go` — NVIDIA via NVML on Windows. A thin
  `syscall.NewLazyDLL("nvml.dll")` wrapper of the ~12 flat C functions
  needed (the Go equivalent of what pynvml does via ctypes). The DLL is
  only touched when the backend is constructed, so machines without
  an NVIDIA driver never load it. VRAM uses `nvmlDeviceGetMemoryInfo_v2`
  (its `used` field matches nvidia-smi's `memory.used`; the v1 struct
  is `{total, free, used}` and its `used` also counts driver-reserved
  memory) with a v1 fallback for older drivers. NVML reports fan speed
  in percent (no RPM query exists), hence `fanPct` vs the AMD `fanRpm`.
- `gpu_amd_linux.go` — AMD via sysfs on Linux, ported from
  strata-monitor.sh: the amdgpu hwmon temperatures (edge/junction/mem),
  fan RPM, microwatt power, `gpu_busy_percent`, `mem_info_vram_*` and
  the `pp_dpm_*` clock tables are plain file reads — deliberately not
  `amd-smi` (whose multi-query SMU access correlates with the wedge
  documented in the amdgpu-stuck-watchdog README). The `temp1_input`
  canary rules inherited from strata: when it fails, every GPU field
  of that card is omitted (a dead SMU costs one blocked read, not
  eight). Only vendor `0x1002` cards are watched.
- `sys_linux.go` — CPU utilization from `/proc/stat` deltas, package
  temperature from the coretemp/k10temp hwmon sensors, RAM from
  `/proc/meminfo`, NVMe throughput from `/proc/diskstats` sector
  deltas paired with the hwmon `nvme` composite temperature by
  controller (`nvme0n1` → `nvme0`).
- `sys_windows.go` — CPU utilization from `GetSystemTimes` deltas,
  RAM from `GlobalMemoryStatusEx`, NVMe drive list from the DOS device
  namespace and throughput from `IOCTL_DISK_PERFORMANCE` deltas (the
  perfmon disk counters). The IOCTL needs an elevated process; when
  unprivileged, drives are still listed with the throughput fields
  omitted. CPU temperature and NVMe temperatures are deliberately not
  attempted on Windows ("omit where unavailable").
- `gpu_nvml_stub.go` / `gpu_amd_stub.go` — the other platforms' nil
  backends. Linux + NVIDIA is a deliberate future extension point
  (NVML via purego, or the nvidia-smi subprocess).

**Logging** (`writer.go`): when `logDir` is configured, each sample is
one JSONL line in `monitor-YYYY-MM-DD.jsonl` (one file per calendar
day, so the daily/weekly windows map onto the newest file and the last
seven files), `Sync`ed after every line (the log is written on the
assumption it will be read after a hard reset). The model manager's
new `Options.OnEvent` callback (fired on load, unload, swap and crash
exit) writes `model_loaded`/`model_unloaded` records into the same
stream and keeps the samples' `model` field current (model key or
`"idle"`). The monitor also writes one `app_started` record (with the
version, written at startup via `Monitor.AppStarted`) and one
`poweroff_initiated` / `poweroff_failed` record per executed/failed
poweroff command (via `Monitor.PoweroffAction`, wired from the
poweroff counter's action callback); all of these are no-ops when no
logDir is configured. At every midnight roll, files older than
`retentionDays` (default 7) are deleted.

**Aggregation** (`stats.go`): `combine` reduces samples into one
sample-shaped result — every numeric field independently averaged or
maximized over the samples where it is present (absent everywhere →
stays absent); GPUs are paired by `Index`, NVMe devices by `Name`
(across samples, so devices appearing/disappearing keep their own
rows). It is implemented by reflection over the sample structs, so
new fields participate automatically. Log files are decoded with an
mtime/size cache keyed per file, keeping repeated endpoint polls off
the disk.

Hardware tests are opt-in (`LLAMA_SWAPPY_E2E=1` env var): the NVML
and Windows/proc backends run against the real machine, the AMD sysfs
backend has fake-sysfs unit tests (Linux only) plus a real-hardware
e2e, and everything else (config validation, writer rotation and
retention with a fake clock, aggregation, endpoint shapes, manager
event wiring) is covered by hermetic unit tests.

## Config

YAML with five top-level keys (see `example/llama-swap-config.yaml`):

- `globalTTL` — idle seconds before unload
- `startPort` — single port reused by whichever model is active; `${PORT}`
  in each model's `cmd` and `proxy` URL is substituted with it
- `models` — map of model key -> `{name, cmd, proxy, maxTokens?}`
- `monitor` — optional `{enabled, logDir?, interval?, retentionDays?}`
  hardware monitoring section (see above)
- `poweroff` — optional `{enabled, idleSeconds, command?}` idle poweroff
  section (see above); enabled requires `idleSeconds > 0` (no default),
  and an enabled section without `command` on non-Linux platforms is a
  startup error

Validation is in `config.Validate` (a disabled monitor or poweroff
section is always valid).

## Package layout

- `main.go` — entrypoint
- `build.ps1`, `build.sh` — build helper scripts (wrappers around
  `go build`; optional output path argument)
- `cmd/root.go` — cobra CLI: `--config` (required), `--listen`, `--version`
- `cmd/version.go` — version constant (`Version`)
- `internal/config` — YAML load/validate
- `internal/inspect` — model metadata parsing from run scripts
- `internal/model` — model lifecycle (start, readiness, idle, stop;
  `Options.OnEvent` lifecycle callback, multiplexed by the CLI to the
  monitor and the poweroff counter; `SplitCommandLine` is also used by
  the poweroff command runner)
- `internal/monitor` — optional hardware heartbeat: platform backends
  (NVML/sysfs), JSONL writer with daily rotation and retention,
  sample aggregation, endpoint status, application-side records
  (model events, app start, poweroff actions)
- `internal/poweroff` — optional idle poweroff: event-driven countdown,
  platform safety check (`/proc` scan on Linux), poweroff command
  execution, status for `/llama-swappy/info`
- `internal/proxy` — OpenAI listener + reverse proxy + info/unload/monitor endpoints
- `internal/version` — application version constant
