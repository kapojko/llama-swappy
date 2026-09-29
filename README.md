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
  "version": "1.2",
  "current": {
    "key": "qwen3.8-27b",
    "name": "Qwen3.8-27B",
    "secondsToUnload": 843
  },
  "models": [
    {
      "key": "qwen3.8-27b",
      "name": "Qwen3.8-27B",
      "contextSize": 114688,
      "maxTokens": 32768,
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
  `reasoning` from `--reasoning`, and `maxTokens` from an optional
  per-model `maxTokens` config value, else the script's `--max-tokens`,
  else `min(32768, contextSize/2)`. `input` is currently hardcoded to
  `["text"]`. Fields that could not be derived (script missing, not a
  text file, flag absent) are omitted.
- `GET /v1/models` returns the standard OpenAI model list with the
  configured model keys.

## Config format

```yaml
# Unload the model after 900 seconds without requests
globalTTL: 900

# Port the active model's llama-server listens on (${PORT} is substituted)
startPort: 12390

models:
  qwen3.8-27b:
    name: "Qwen3.8-27B"
    cmd: env PORT=${PORT} /home/yury/run_qwen3.8_27b.sh
    proxy: http://127.0.0.1:${PORT}
```

Only `globalTTL`, `startPort`, and `models` (with per-model `name`, `cmd`,
`proxy` and optional `maxTokens`) are supported. `${PORT}` in `proxy` is
always replaced with
`startPort` — the single active model always uses that port.

### Windows note

`cmd` is executed as a plain command line without a shell. On Linux the
example works as-is; on Windows provide an executable invocation, e.g.

```yaml
cmd: cmd /c "set PORT=%PORT% & llama-server.exe ..."
```

## Behavior

- **Start**: on a request for a model that is not loaded, the current model
  (if any) is stopped, the new model's `cmd` is spawned, and the app polls
  `<proxy>/health` until it is ready (timeout: 5 minutes; on timeout the
  model is unloaded and the request gets a 503 — the next request retries).
- **Proxy**: the request is reverse-proxied to the model's `proxy` URL and
  streamed back.
- **Idle unload**: if no requests arrive for `globalTTL` seconds, the model
  is stopped (on Unix: SIGTERM to its whole process group, escalating to
  SIGKILL after a 10s grace; on Windows: the direct child is killed).
- **Crash recovery**: if the model process exits unexpectedly (e.g.
  segfault), the exit is detected, the model is cleared, and it is
  auto-restarted after 10 seconds — or immediately on the next request in
   the meantime. Auto-restart is suppressed after 3 consecutive crashes
   that never served a 2xx response; any subsequent request starts the
   model and begins a fresh crash streak.
- **Output**: the model process's stdout/stderr go to llama-swappy's
  stdout; llama-swappy's own logs go to stderr.
- **Shutdown**: Ctrl+C stops the server and unloads any loaded model.

## Limitations

- On Unix, unloading sends SIGTERM to the model's whole process group
  (the wrapper script and everything it spawns), waiting 10s before
  escalating to SIGKILL — so a plain wrapper script is stopped
  completely, and the server gets its clean-exit path on SIGTERM. On
  Windows only the direct child is killed; use a direct executable
  invocation there. A grandchild that leaves the process group (e.g.
  via `setsid` or daemonizing) is not stopped. The unload wait is
  bounded (30s), so this cannot hang the process.
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
