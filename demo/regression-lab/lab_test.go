package main

import (
	"testing"
	"time"
)

func TestBurnIsDeterministicAndUsed(t *testing.T) {
	a, b := burn(10_000), burn(10_000)
	if a == 0 || a != b {
		t.Fatalf("burn(10000) = %d, %d", a, b)
	}
	if burn(10_001) == a {
		t.Fatal("different iteration counts produced the same mix")
	}
}

func TestCalibrateItersPositive(t *testing.T) {
	n := calibrateIters(200 * time.Microsecond)
	if n < 1 {
		t.Fatalf("calibrateIters = %d", n)
	}
}

func TestCalibrateItersRespectsMax(t *testing.T) {
	n := calibrateIters(24 * time.Hour)
	if n < 1 || n > maxIters {
		t.Fatalf("calibrateIters = %d, want 1..%d", n, maxIters)
	}
}

func TestParseRejectsUnknownArgs(t *testing.T) {
	if _, err := parseArgs([]string{"extra"}); err == nil {
		t.Fatal("unexpected argument accepted")
	}
}
