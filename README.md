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
`proxy`) are supported. `${PORT}` in `proxy` is always replaced with
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
  process is killed.
- **Output**: the model process's stdout/stderr go to llama-swappy's
  stdout; llama-swappy's own logs go to stderr.
- **Shutdown**: Ctrl+C stops the server and unloads any loaded model.

## Limitations

- Unloading kills only the direct child process. If your `cmd` is a shell
  script that spawns the server as a grandchild, make sure the script
  forwards termination (e.g. trap) or use `exec`. The unload wait is
  bounded (15s), so this cannot hang the process.
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
process spawning, stdout capture, idle unload, and the full
config -> proxy pipeline over a real HTTP listener.
