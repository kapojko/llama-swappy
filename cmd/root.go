package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"llama-swappy/internal/backup"
	"llama-swappy/internal/config"
	"llama-swappy/internal/inspect"
	"llama-swappy/internal/model"
	"llama-swappy/internal/monitor"
	"llama-swappy/internal/poweroff"
	"llama-swappy/internal/proxy"
)

var (
	configPath string
	listen     string
)

var rootCmd = &cobra.Command{
	Use:     "llama-swappy",
	Short:   "llama-swappy - swaps llama.cpp models behind an OpenAI-compatible proxy",
	Version: Version,
	Long: "llama-swappy listens for OpenAI-compatible requests, starts the requested " +
		"model on demand, proxies the request to it, and unloads the model after a " +
		"period of inactivity. Only one model is loaded at a time.",
	RunE: func(_ *cobra.Command, _ []string) error {
		return run(context.Background(), configPath, listen)
	},
}

func init() {
	rootCmd.Flags().StringVar(&configPath, "config", "", "path to the YAML config file (required)")
	rootCmd.Flags().StringVar(&listen, "listen", "127.0.0.1:12380", "address to listen for OpenAI requests on")
	_ = rootCmd.MarkFlagRequired("config")
}

// Execute runs the CLI.
func Execute() error {
	return rootCmd.Execute()
}

// syncConfigBackup mirrors the current configuration (the config file
// and every referenced launch script) into the per-machine backup
// directory. It is best-effort: every problem is logged as a warning
// and never fails startup.
func syncConfigBackup(log *slog.Logger, cfg *config.Config, cfgPath string) {
	machineName := cfg.Backup.MachineName
	if machineName == "" {
		machineName = backup.DefaultMachineName()
	}
	sources, warns := backup.SourcesForConfig(cfg, cfgPath)
	for _, w := range warns {
		log.Warn("config backup: "+w)
	}
	// Drop sources whose current file is missing on disk (e.g. a script
	// that was deleted but not yet removed from the config): it cannot be
	// backed up, and its stale copy (if any) is treated as outdated.
	kept := make([]backup.Source, 0, len(sources))
	for _, s := range sources {
		if _, err := os.Stat(s.Src); err != nil {
			log.Warn("config backup: source not found, skipping", "file", s.Src)
			continue
		}
		kept = append(kept, s)
	}
	err := backup.Sync(backup.Options{
		Dir:         cfg.Backup.Dir,
		MachineName: machineName,
		Sources:     kept,
	}, log)
	if err != nil {
		log.Warn("config backup: sync failed", "err", err)
	}
}

func run(ctx context.Context, cfgPath, listen string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// Version first, so logs from a long-running instance (or a bug
	// report) always say which build produced them.
	log.Info("llama-swappy version", "version", Version)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// Parse each model's run script up front so the metadata is logged
	// at startup and available for /llama-swappy/info. A model whose
	// script cannot be read is still usable; it is just reported without
	// the derived fields.
	infos := make(map[string]inspect.Info, len(cfg.Models))
	for key, m := range cfg.Models {
		info, err := inspect.Inspect(m.Cmd, cfg.StartPort)
		if err != nil {
			log.Warn("model metadata: parse failed", "model", key, "err", err)
			continue
		}
		infos[key] = info
		attrs := []any{"model", key, "name", m.Name}
		if info.ContextSize > 0 {
			attrs = append(attrs, "contextSize", info.ContextSize)
		}
		if info.Slots > 0 {
			attrs = append(attrs, "slots", info.Slots)
		}
		if mt := inspect.MaxTokens(m.MaxTokens, info); mt > 0 {
			attrs = append(attrs, "maxTokens", mt)
		}
		if info.Reasoning != nil {
			attrs = append(attrs, "reasoning", *info.Reasoning)
		}
		if info.Vision != nil {
			attrs = append(attrs, "vision", *info.Vision)
		}
		log.Info("model metadata", attrs...)
	}
	// Config backup is fully optional and runs once at startup: it
	// mirrors the config file and every referenced launch script into
	// <dir>/<machineName>/. A failure never prevents the app from
	// starting — it is logged and ignored.
	if cfg.Backup != nil && cfg.Backup.Enabled {
		syncConfigBackup(log, cfg, cfgPath)
	}
	// The monitor is fully optional: when the config has no enabled
	// monitor section, monitor.New returns nil and no hardware is
	// touched at all. ModelEvent is nil-safe, so the manager wiring is
	// the same either way.
	mon, err := monitor.New(cfg.Monitor, log)
	if err != nil {
		return fmt.Errorf("monitor: %w", err)
	}
	if mon != nil {
		defer mon.Stop()
		mon.Start()
	}
	// The poweroff counter is fully optional: when the config has no
	// enabled poweroff section, poweroff.New returns nil and no
	// shutdown is ever attempted. An enabled section without a command
	// on a non-Linux platform is a startup error (like a bad config).
	po, err := poweroff.New(cfg.Poweroff, log)
	if err != nil {
		return fmt.Errorf("poweroff: %w", err)
	}
	if po != nil {
		defer po.Close()
		// The executed/failed poweroff command is also written into
		// the monitor's JSONL log (no-op when monitoring or its log
		// dir is disabled).
		po.SetAction(mon.PoweroffAction)
		log.Info("idle poweroff enabled", "idleSeconds", cfg.Poweroff.IdleSeconds, "command", po.Command())
	}
	// The manager's lifecycle events feed the monitor log and the
	// poweroff counter. OnEvent runs with the manager's lock held, so
	// the handlers must not block or call back into the manager.
	onEvent := func(action, key string) {
		mon.ModelEvent(action, key)
		switch action {
		case model.EventLoaded:
			po.OnLoaded()
		case model.EventUnloaded:
			po.OnUnloaded()
		}
	}
	mgr := model.New(cfg, model.Options{Logger: log, Out: os.Stdout, OnEvent: onEvent})
	defer mgr.Close()
	server := proxy.New(cfg, mgr, log, infos, mon, po)

	// The app start is the first application-side record of the
	// monitor's JSONL log (no-op when monitoring is disabled).
	mon.AppStarted(Version)

	httpSrv := &http.Server{Addr: listen, Handler: server}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", listen, "models", len(cfg.Models))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// SIGTERM is not a supported signal on Windows; fall back to Ctrl+C only.
	signals := []os.Signal{os.Interrupt}
	if runtime.GOOS != "windows" {
		signals = append(signals, syscall.SIGTERM)
	}
	sigCtx, stop := signal.NotifyContext(ctx, signals...)
	defer stop()
	select {
	case <-sigCtx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		return fmt.Errorf("server: %w", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	return nil
}
