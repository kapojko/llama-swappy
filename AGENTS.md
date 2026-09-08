# llama-swappy - minified version of llama-swap

CLI utility that runs llama.cpp server proxying OpenAI API for different models, and unloads it after period of standby. Only one model is loaded at a time; a request for another model swaps it in. No UI, no GPU interaction.

## Dependencies

- github.com/spf13/cobra - CLI framework
- gopkg.in/yaml.v3 - config parsing
- everything else is standard library (net/http, httputil, os/exec, log/slog)

## Repository Structure

- docs/CODE.md — high-level code description
- main.go — entrypoint
- cmd/root.go — cobra CLI (`--config`, `--listen`)
- internal/config — YAML config load/validate
- internal/model — model lifecycle: start, readiness poll, idle unload, stop
- internal/proxy — OpenAI request listener + reverse proxy
- internal/testutil — integration test helpers (fake server build, free port)
- example/llama-swap-config.yaml — example config
- README.md — usage documentation

## Build, Test & Run

```bash
go build -o llama-swappy.exe .
go test ./...
llama-swappy.exe --config "$CONFIG_PATH" --listen 0.0.0.0:12380
```

## Coding Guidelines

- Follow standard Go style (gofmt, goimports) and Effective Go guidelines.
- Handle errors explicitly; do not ignore errors (\_ = ... only if strictly necessary).
- Use structured logging (e.g., slog or zerolog) rather than fmt.Printf for output.
- Keep functionality in separate packages/files (config, model, proxy, cmd).
- Keep the CLI surface minimal: only `--config`, `--listen` and help.

## Important Rules

- When modifying code structure, always update `docs/CODE.md` to reflect changes.
- NEVER commit without explicit order
