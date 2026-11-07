// Command regression-lab is a loopback-only HTTP target that does real CPU
// work on GET /work. After a documented delay from the first /work request it
// switches from a light baseline loop to a heavier one, so Ruptiq can record
// a controlled change. Prometheus metrics are at GET /metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	defaultAddr         = "127.0.0.1:8090"
	defaultDegradeAfter = 10 * time.Second
	baselineTarget      = 250 * time.Microsecond
	degradedTarget      = 12 * time.Millisecond
	shutdownTimeout     = 10 * time.Second
)

type labConfig struct {
	addr          string
	degradeAfter  time.Duration
	baselineIters int
	degradedIters int
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if err := run(logger, os.Args[1:]); err != nil {
		logger.Error("regression-lab exited with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, args []string) error {
	cfg, err := parseArgs(args)
	if err != nil {
		return err
	}
	if cfg.baselineIters == 0 {
		cfg.baselineIters = calibrateIters(baselineTarget)
	}
	if cfg.degradedIters == 0 {
		cfg.degradedIters = calibrateIters(degradedTarget)
	}
	if cfg.degradedIters <= cfg.baselineIters {
		return fmt.Errorf("degraded iterations (%d) must be greater than baseline (%d)", cfg.degradedIters, cfg.baselineIters)
	}

	_, handler := newLabHandler(cfg, logger)
	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("regression-lab listening",
			"addr", cfg.addr,
			"degrade_after", cfg.degradeAfter.String(),
			"baseline_iters", cfg.baselineIters,
			"degraded_iters", cfg.degradedIters,
		)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("regression-lab stopped")
	return nil
}

func parseArgs(args []string) (labConfig, error) {
	fs := flag.NewFlagSet("regression-lab", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var c labConfig
	fs.StringVar(&c.addr, "addr", defaultAddr, "listen `address` (loopback only)")
	fs.DurationVar(&c.degradeAfter, "degrade-after", defaultDegradeAfter, "time after the first GET /work before switching to degraded CPU work")
	fs.IntVar(&c.baselineIters, "baseline-iters", 0, "CPU-loop iterations per GET /work in baseline mode (0: calibrate to ~250µs; max 50000000)")
	fs.IntVar(&c.degradedIters, "degraded-iters", 0, "CPU-loop iterations per GET /work in degraded mode (0: calibrate to ~12ms; max 50000000)")
	if err := fs.Parse(args); err != nil {
		return labConfig{}, err
	}
	if fs.NArg() > 0 {
		return labConfig{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	if c.degradeAfter < 0 {
		return labConfig{}, fmt.Errorf("-degrade-after must be non-negative, got %s", c.degradeAfter)
	}
	if c.baselineIters < 0 || c.degradedIters < 0 {
		return labConfig{}, fmt.Errorf("iteration counts must be non-negative")
	}
	if c.baselineIters > maxIters || c.degradedIters > maxIters {
		return labConfig{}, fmt.Errorf("iteration counts must be at most %d", maxIters)
	}
	if err := requireLoopback(c.addr); err != nil {
		return labConfig{}, err
	}
	return c, nil
}

func requireLoopback(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address: %w", err)
	}
	if port == "" {
		return fmt.Errorf("listen address %q has no port", addr)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address must be loopback (127.0.0.1, ::1, or localhost), got %q", host)
	}
	return nil
}
