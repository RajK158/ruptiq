// Command order-service runs the checkout demo's Order Service.
//
// This is a demo target: the system under test that Ruptiq will load test.
// It must never import Ruptiq Core packages.
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

	"github.com/RajK158/ruptiq/demo/checkout/order-service/internal/httpapi"
	"github.com/RajK158/ruptiq/demo/checkout/order-service/internal/inventoryclient"
	"github.com/RajK158/ruptiq/demo/checkout/order-service/internal/order"
	"github.com/RajK158/ruptiq/demo/checkout/order-service/internal/paymentclient"
)

const (
	defaultPort     = "8081"
	shutdownTimeout = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("order-service exited with error", "error", err)
		os.Exit(1)
	}
}

// run wires everything together and blocks until the server stops.
func run() error {
	port, err := portFromEnv(os.Getenv("ORDER_SERVICE_PORT"))
	if err != nil {
		return err
	}

	inventory, err := inventoryClientFromEnv(os.Getenv("INVENTORY_SERVICE_URL"))
	if err != nil {
		return err
	}

	payments, err := paymentClientFromEnv(os.Getenv("PAYMENT_SERVICE_URL"))
	if err != nil {
		return err
	}

	srv := httpapi.NewServer(":"+port, order.NewStore(), inventory, payments)

	// ctx is cancelled when the process receives Ctrl+C (SIGINT) or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Run the server in a goroutine so this function can wait for a signal.
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("order-service listening", "addr", srv.Addr,
			"inventory_service_url", inventory.BaseURL(), "payment_service_url", payments.BaseURL())
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
	slog.Info("order-service stopped")
	return nil
}

// inventoryClientFromEnv builds the Inventory Service client from value, or
// from the default URL when value is empty. An invalid URL is an error, so a
// typo stops the service at startup instead of failing later on every reserve.
func inventoryClientFromEnv(value string) (*inventoryclient.Client, error) {
	if value == "" {
		value = inventoryclient.DefaultBaseURL
	}
	c, err := inventoryclient.New(value, inventoryclient.DefaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid INVENTORY_SERVICE_URL: %w", err)
	}
	return c, nil
}

// paymentClientFromEnv builds the Payment Service client from value, or from
// the default URL when value is empty. An invalid URL stops startup.
func paymentClientFromEnv(value string) (*paymentclient.Client, error) {
	if value == "" {
		value = paymentclient.DefaultBaseURL
	}
	c, err := paymentclient.New(value, paymentclient.DefaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid PAYMENT_SERVICE_URL: %w", err)
	}
	return c, nil
}

// portFromEnv returns value as a validated port, or the default when empty.
func portFromEnv(value string) (string, error) {
	if value == "" {
		return defaultPort, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid ORDER_SERVICE_PORT %q: must be a number between 1 and 65535", value)
	}
	// Return the parsed number, not the input: Atoi accepts forms such as
	// "+80" that are not valid in a listen address.
	return strconv.Itoa(n), nil
}
