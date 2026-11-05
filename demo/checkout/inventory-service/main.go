// Command inventory-service runs the checkout demo's Inventory Service.
//
// This is a demo target: the system under test that Ruptiq will load test.
// It must never import Ruptiq Core packages or the Order Service.
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

	"github.com/RajK158/ruptiq/demo/checkout/inventory-service/internal/httpapi"
	"github.com/RajK158/ruptiq/demo/checkout/inventory-service/internal/inventory"
)

const (
	defaultPort     = "8082"
	shutdownTimeout = 10 * time.Second
)

func main() {
	// JSON lines on stdout: easy for log tools to parse later.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("inventory-service exited with error", "error", err)
		os.Exit(1)
	}
}

// run wires everything together and blocks until the server stops.
func run(logger *slog.Logger) error {
	port, err := portFromEnv(os.Getenv("INVENTORY_SERVICE_PORT"))
	if err != nil {
		return err
	}

	// The seed catalog is demo/test data only; see inventory.SeedStock.
	srv := httpapi.NewServer(":"+port, inventory.NewStore(inventory.SeedStock()), logger)

	// ctx is cancelled when the process receives Ctrl+C (SIGINT) or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Run the server in a goroutine so this function can wait for a signal.
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("inventory-service listening", "addr", srv.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		// The server stopped on its own (for example, the port is in use).
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
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
	logger.Info("inventory-service stopped")
	return nil
}

// portFromEnv returns value as a validated port, or the default when empty.
func portFromEnv(value string) (string, error) {
	if value == "" {
		return defaultPort, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid INVENTORY_SERVICE_PORT %q: must be a number between 1 and 65535", value)
	}
	// Return the parsed number, not the input: Atoi accepts forms such as
	// "+80" that are not valid in a listen address.
	return strconv.Itoa(n), nil
}
