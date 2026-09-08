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

	"llama-swappy/internal/config"
	"llama-swappy/internal/model"
	"llama-swappy/internal/proxy"
)

var (
	configPath string
	listen     string
)

var rootCmd = &cobra.Command{
	Use:   "llama-swappy",
	Short: "llama-swappy - swaps llama.cpp models behind an OpenAI-compatible proxy",
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

func run(ctx context.Context, cfgPath, listen string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	mgr := model.New(cfg, model.Options{Logger: log, Out: os.Stdout})
	defer mgr.Close()
	server := proxy.New(cfg, mgr, log)

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
