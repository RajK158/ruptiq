// Command ruptiq-api starts the Ruptiq HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/RajK158/ruptiq/internal/api"
)

const (
	defaultPort     = "8080"
	shutdownTimeout = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("ruptiq-api exited with error", "error", err)
		os.Exit(1)
	}
}

// run wires everything together and blocks until the server stops.
// Keeping this out of main() lets us return errors instead of exiting midway.
func run() error {
	port, err := portFromEnv(os.Getenv("PORT"))
	if err != nil {
		return err
	}

	srv := api.NewServer(":" + port)

	// ctx is cancelled when the process receives Ctrl+C (SIGINT) or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Run the server in a goroutine so this function can wait for a signal.
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("ruptiq-api listening", "addr", srv.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		// The server stopped on its own (for example, the port is in use).
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	}

	// Stop accepting new connections and give in-flight requests time to finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	// After Shutdown, ListenAndServe returns http.ErrServerClosed. That is expected.
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	slog.Info("ruptiq-api stopped")
	return nil
}

// portFromEnv returns value as a validated port, or the default when empty.
func portFromEnv(value string) (string, error) {
	if value == "" {
		return defaultPort, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid PORT %q: must be a number between 1 and 65535", value)
	}
	// Return the parsed number, not the input: Atoi accepts forms such as
	// "+80" that are not valid in a listen address.
	return strconv.Itoa(n), nil
}
