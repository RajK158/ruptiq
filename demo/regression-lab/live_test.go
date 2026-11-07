package main

import (
	"os"
	"os/exec"
	"testing"
)

// Live k6 validation is opt-in so the default suite and GitHub CI never need
// k6. The commands are in README.md. Setting RUPTIQ_LIVE_K6=1 only checks
// that a k6 executable is visible; it does not start a 20s load test from
// `go test`.
func TestLiveK6IsOptIn(t *testing.T) {
	if os.Getenv("RUPTIQ_LIVE_K6") == "" {
		t.Skip("live k6 pipeline is documented in README.md; set RUPTIQ_LIVE_K6=1 to confirm k6 is on PATH or RUPTIQ_K6_PATH")
	}
	k6 := os.Getenv("RUPTIQ_K6_PATH")
	if k6 == "" {
		var err error
		k6, err = exec.LookPath("k6")
		if err != nil {
			t.Skip("RUPTIQ_LIVE_K6 is set but k6 was not found; set RUPTIQ_K6_PATH to a portable k6.exe")
		}
	}
	if _, err := os.Stat(k6); err != nil {
		t.Fatalf("k6 at %s: %v", k6, err)
	}
	t.Log("k6 is available at", k6)
}
