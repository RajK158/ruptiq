// Package api contains the HTTP server and handlers for the Ruptiq API.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// serviceName is reported by the health endpoint so callers can tell which
// service answered.
const serviceName = "ruptiq-api"

// healthResponse is the JSON body returned by GET /healthz.
type healthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
}

// NewHandler returns the HTTP handler that serves every Ruptiq API route.
//
// Routes use the "METHOD /path" pattern syntax from Go 1.22+. A request with
// a different method (for example POST /healthz) automatically receives
// "405 Method Not Allowed".
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	return mux
}

// NewServer returns an http.Server listening on addr with conservative
// timeouts. Without timeouts, slow or stalled clients can hold connections
// open forever.
func NewServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           NewHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// handleHealthz reports that the process is up and able to serve requests.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(healthResponse{Status: "ok", Service: serviceName}); err != nil {
		// The status line is already sent, so all we can do is log.
		slog.Error("failed to write healthz response", "error", err)
	}
}
