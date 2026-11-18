# llama-swappy

A minimal Go CLI that proxies OpenAI-compatible requests to llama.cpp
servers, starting the requested model on demand and unloading it after a
period of inactivity. Only one model is loaded at a time — a request for a
different model swaps the current one out.

Unlike the original llama-swap there is no UI and no GPU interaction:
llama-swappy only spawns the model command you give it and proxies HTTP.

## Build

```bash
go build -o llama-swappy.exe .
```

or use the helper scripts, which accept an optional output path:

- Windows: `build.ps1` (default output `llama-swappy.exe`), e.g.
  `.\build.ps1 -OutPath D:\some\dir\llama-swappy.exe`
- Linux: `build.sh` (default output `llama-swappy`), e.g. `./build.sh /some/dir/llama-swappy`

## Run

```bash
llama-swappy --config "$CONFIG_PATH" --listen 0.0.0.0:12380
```

Flags (only these two are supported, plus `--help`):

| flag | required | default | description |
|------|----------|---------|-------------|
| `--config` | yes | — | path to the YAML config file |
| `--listen` | no | `127.0.0.1:12380` | address to listen for OpenAI requests on |

Point any OpenAI-compatible client at `http://<listen-address>` and set the
`model` field to a model key from your config.

## Self-description endpoints

llama-swappy also serves its own metadata, for tools (e.g. the pi agent
provider extension) that want to configure models automatically:

```
GET /llama-swappy/info
```

```json
{
  "version": "1.3",
  "current": {
    "key": "qwen3.8-27b",
    "name": "Qwen3.8-27B",
    "secondsToUnload": 843
  },
  "models": [
    {
      "key": "qwen3.8-27b",
      "name": "Qwen3.8-27B",
      "contextSize": 36864,
      "slots": 4,
      "maxTokens": 18432,
      "reasoning": true,
      "input": ["text"]
    }
  ]
}
```

- `current` is `null` when no model is loaded; `secondsToUnload` is the
  remaining idle time before the model is unloaded.
- Per-model fields are derived at startup by parsing the model's run
  script (or the `cmd` line itself when there is no wrapper script):
  `contextSize` from `-c`/`--ctx-size` (last uncommented occurrence),
  `slots` from `-np`/`--parallel`, `reasoning` from `--reasoning`, and
  `maxTokens` from an optional per-model `maxTokens` config value, else
  the script's `--max-tokens`, else `min(32768, contextSize/2)`.
  Because llama-server divides the total `-c` value equally among the
  parallel slots, `contextSize` is the per-request context (total
  divided by `slots`); without `-np` llama-server's own default applies
  (single slot on older builds, auto on newer ones). `input` lists the
  modalities the model accepts: `["text"]`, plus `"image"` when the
  run script declares an image reader — llama-server's `-mm`/`--mmproj`
  (an mmproj file) or a runner's `--vision` (Strata: bare, a device
  name, or on/off). Fields that could not be derived
  (script missing, not a text file, flag absent) are omitted.
- `GET /v1/models` returns the standard OpenAI model list with the
  configured model keys.
- `POST /llama-swappy/unload` unloads the currently loaded model
  immediately, without stopping the proxy (the next request restarts the
  model). The response reports which model was stopped:

  ```json
  {"unloaded": "qwen3.8-27b"}
  ```

  `unloaded` is `null` when no model was loaded. Handy to free VRAM on
  demand, e.g. `curl -X POST http://127.0.0.1:12380/llama-swappy/unload`.

## Hardware monitoring (optional)

An optional heartbeat sampler modeled after the strata-monitor box
heartbeat monitors 0-N GPUs, the CPU, RAM
and all NVMe devices, and (when a `logDir` is configured) writes one
JSONL line per sample plus `model_loaded`/`model_unloaded` records to
`monitor-YYYY-MM-DD.jsonl` — so the hardware timeline and the model
timeline share one file that survives crashes (every line is synced to
disk). It is fully opt-in: without an enabled `monitor` section in the
config no hardware is accessed at all.

```yaml
monitor:
  enabled: true
  logDir: /var/log/llama-swappy   # optional; no logs without it
  interval: 5s                    # default 5s
  retentionDays: 7                # default 7
```

Per platform, the data sources are the cheap ones: NVML
(`nvml.dll`/`libnvidia-ml`, loaded lazily — never touched without an
NVIDIA driver, never touched at all when monitoring is disabled) for
NVIDIA GPUs on Windows, and plain sysfs reads for AMD GPUs on Linux
(the amdgpu driver publishes temperatures, fan, power, utilization,
VRAM and DPM clocks as files — no process is spawned and the
multi-query `amd-smi` interface is deliberately never used). CPU
utilization comes from the kernel CPU-time counters, RAM from the
OS memory status, NVMe throughput from the kernel disk counters.
Values a platform or driver does not offer are simply omitted (e.g.
CPU temperature and NVMe temperatures on Windows without elevation).

```GET /llama-swappy/monitor``` answers (always registered — with
monitoring disabled it returns `200` with `{"enabled":false,
"reason":"..."}`, so consumers can tell "monitoring off" from "old
app without the endpoint"):

```json
{
  "enabled": true,
  "now": { "ts": "...", "gpus": [...], "cpu": {...}, "ram": {...},
           "nvmes": [...], "model": "idle" },
  "avgMinute": { ... },
  "maxDay": { ... },
  "maxWeek": { ... },
  "historyAvailable": true
}
```

`now` is the newest sample, `avgMinute` the average over the last
minute (in memory), `maxDay`/`maxWeek` the maxima over today's log
file and the last seven files (omitted when logging is not
configured). GPU samples are paired by index and NVMe samples by
name across samples, so 0-N devices are tracked individually. Fields:
per GPU — temperature(s), utilization %, VRAM used/total MiB, power W,
fan (rpm on AMD, % on NVIDIA), core/memory clock current/max MHz (the
pair is the throttle witness); CPU — utilization %, temperature;
RAM — used/total and swap used/total MiB; per NVMe — temperature,
read/write B/s.

A probe failure never stops the sampler — the affected field is
omitted for that sample, and a permanently unavailable value is
logged once, not once per sample.

## Native llama-server endpoints (`/llama/...`)

A small set of native llama-server endpoints is exposed under the
`/llama` prefix; the proxy strips the prefix and forwards the request to
the requested model's llama-server. These are tokenizer/template
helpers: they never run prefill or decode. The `model` field in the
request body still selects the model (and starts or swaps it in, like
any other request):

```
POST /llama/tokenize    {"model": "qwen3.8-27b", "content": "Hello", "add_special": true}
POST /llama/detokenize  {"model": "qwen3.8-27b", "tokens": [9707, 615]}
POST /llama/apply-template  {"model": "qwen3.8-27b", "messages": [...]}
```

Anything else under `/llama/` is rejected with 404. Because the exposed
endpoints are whitelisted in the proxy itself, a fronting reverse proxy
(e.g. nginx) only needs one stable rule to expose them:

```nginx
location ~ ^/(v1|llama-swappy|llama)/ {
    proxy_pass http://127.0.0.1:12380;
    ...
}
```

## Config format

```yaml
# Unload the model after 900 seconds without requests
globalTTL: 900

# Port the active model's llama-server listens on (${PORT} is substituted)
startPort: 12390

# Optional hardware monitoring (see "Hardware monitoring")
monitor:
  enabled: true
  logDir: /var/log/llama-swappy
  interval: 5s
  retentionDays: 7

models:
  qwen3.8-27b:
    name: "Qwen3.8-27B"
    cmd: env PORT=${PORT} /home/yury/run_qwen3.8_27b.sh
    proxy: http://127.0.0.1:${PORT}
```

`globalTTL`, `startPort`, `models` (with per-model `name`, `cmd`,
`proxy` and optional `maxTokens`) and the optional `monitor` section
are supported. `${PORT}` in `proxy` is
always replaced with
`startPort` — the single active model always uses that port.

### Run scripts on Windows (PowerShell)

`cmd` is executed as a plain command line without a shell. On Windows use a
PowerShell wrapper script that takes the port as a parameter:

```yaml
cmd: powershell -NoProfile -ExecutionPolicy Bypass -File C:\run\run_qwen3.8-27b.ps1 -Port ${PORT}
```

The script declares `param([int]$Port = 12380)` and receives llama-swappy's
assigned port as `-Port`; see `example/run_qwen3.8-27b.ps1` for the template.
Do not hardcode the port in the script: it collides with the port
llama-swappy itself listens on and prevents models from sharing `startPort`.

### Metadata hints in run scripts

llama-swappy derives `contextSize`, `slots`, `reasoning`, `maxTokens`
and whether the model reads images (`input`) for `/llama-swappy/info` by
tokenizing every uncommented line of the run script.
When the real flags are hidden inside another tool's config (e.g. a runner
that stores `-c`/`-np` in its own data directory), declare them with a no-op
line that the shell ignores but llama-swappy parses:

```bash
# bash: ':' is a no-op command that ignores its arguments
: -c 115000 -np 1
```

```powershell
# powershell: a stub function collects all arguments without binding
function llama-swappy-metadata {}
llama-swappy-metadata -c 115000 -np 1
```

Keep these values in sync with what the runner actually uses — they are
declarations, not settings. Flag values in such hint lines must be literal
numbers; anything the real script also sets explicitly is overridden by the
last uncommented occurrence in the script. A hint line can also declare an
image reader when the real flag lives in the runner's own config, e.g.
`llama-swappy-metadata --vision` for Strata or
`llama-swappy-metadata -mm "$MMPROJ"` for llama-server.

## Behavior

- **Start**: on a request for a model that is not loaded, the current model
  (if any) is stopped, the new model's `cmd` is spawned, and the app polls
  `<proxy>/health` until it is ready (timeout: 5 minutes). If the process
  exits during the wait (e.g. it failed to load), the start fails
  immediately instead of burning the timeout; the request gets a 503, and
  the start is scheduled for an automatic retry (see below).
- **Proxy**: the request is reverse-proxied to the model's `proxy` URL and
  streamed back.
- **Idle unload**: if no requests arrive for `globalTTL` seconds, the model
  is stopped (on Unix: SIGTERM to its whole process group, escalating to
  SIGKILL after a 10s grace; on Windows: the whole process tree is
  terminated via a job object).
- **Crash recovery / failed-start retry**: if the model process exits
  unexpectedly (e.g. segfault), or a start fails (spawn error, exit or
  timeout during readiness), the model is cleared and auto-restarted
  after 10 seconds — or immediately on the next request in the meantime.
  The retry delay doubles with each consecutive failure (10s → 20s → 40s
  → ... capped at 2 minutes), so transient start failures — e.g. a GPU
  driver that releases the previous model's memory asynchronously —
  self-heal over minutes. Auto-retry is suppressed after 5 consecutive
  failures that never served a 2xx response; any subsequent request
  starts the model and begins a fresh failure streak.
- **Output**: the model process's stdout/stderr go to llama-swappy's
  stdout; llama-swappy's own logs go to stderr.
- **Shutdown**: Ctrl+C stops the server and unloads any loaded model.

## Limitations

- On Unix, unloading sends SIGTERM to the model's whole process group
  (the wrapper script and everything it spawns), waiting 10s before
  escalating to SIGKILL — so a plain wrapper script is stopped
  completely, and the server gets its clean-exit path on SIGTERM. On
  Windows the whole tree is terminated through a kill-on-close job
  object: everything the direct child spawns joins the job, and the
  tree also dies if llama-swappy itself exits without unloading. What
  is *not* stopped is a grandchild that deliberately escapes — one that
  leaves the process group (e.g. via `setsid` or daemonizing) on Unix,
  or breaks out of the job (`CREATE_BREAKAWAY_FROM_JOB`) on Windows.
  The unload wait is bounded (30s), so this cannot hang the process.
- Requests arrive at the proxy URL, not the model port; port conflicts with
  other services on `startPort` are not detected ahead of time.
- If an upstream failure happens after the response has already started
  streaming, the client sees a truncated response (the status code can no
  longer be rewritten).

## Tests

```bash
go test ./...
```

Besides unit tests, the suite includes integration tests that build a
fake "llama-server" with the local go toolchain and exercise the real
process spawning, stdout capture, idle unload — including a wrapper
script that spawns the server as a grandchild (Unix only) — and the
full config -> proxy pipeline over a real HTTP listener, including the
self-description endpoints with real run-script fixtures.
On Windows the Unix-only end-to-end test is re-run inside the default
WSL distro (needs a modern Go toolchain in `$HOME/sdk/go`; skipped if
unavailable).

The monitor backends additionally have opt-in hardware tests, skipped
unless `LLAMA_SWAPPY_E2E=1` is set, so the normal suite stays hermetic:

```bash
# Windows (NVIDIA/NVML, CPU/RAM, NVMe) or Linux/WSL (proc, sysfs):
LLAMA_SWAPPY_E2E=1 go test ./internal/monitor -run E2E -v
```
