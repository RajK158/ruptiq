// Package main is the regression lab: a small HTTP target whose handler does
// a configurable amount of real CPU work. It is a validation fixture for
// Ruptiq's experiment → analysis → investigation pipeline, not a checkout
// service and not Ruptiq Core.
package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	modeBaseline = "baseline"
	modeDegraded = "degraded"
	// maxIters is the hard cap on CPU-loop iterations per GET /work. It
	// bounds both calibration and -baseline-iters / -degraded-iters so a
	// typo cannot pin a core for an unbounded burn.
	maxIters = 50_000_000
)

// Lab holds the work mode. The first GET /work starts a timer; after
// DegradeAfter it switches to the heavier iteration count. There is no HTTP
// control for the switch.
type Lab struct {
	degradeAfter  time.Duration
	baselineIters int
	degradedIters int
	degraded      prometheus.Gauge
	iters         prometheus.Gauge

	startOnce  sync.Once
	mu         sync.Mutex
	isDegraded bool
}

func newLab(degradeAfter time.Duration, baselineIters, degradedIters int, degraded, iters prometheus.Gauge) *Lab {
	l := &Lab{
		degradeAfter:  degradeAfter,
		baselineIters: baselineIters,
		degradedIters: degradedIters,
		degraded:      degraded,
		iters:         iters,
	}
	l.degraded.Set(0)
	l.iters.Set(float64(baselineIters))
	return l
}

// onWork is called for every GET /work. It returns the FNV-1a-style mix the
// handler must use, so the CPU loop cannot be deleted as dead code.
func (l *Lab) onWork() (mode string, iters int, mix uint64) {
	l.startOnce.Do(l.startTimer)
	iters = l.currentIters()
	mix = burn(iters)
	if l.degradedMode() {
		return modeDegraded, iters, mix
	}
	return modeBaseline, iters, mix
}

func (l *Lab) startTimer() {
	if l.degradeAfter <= 0 {
		l.enterDegraded()
		return
	}
	go func() {
		time.Sleep(l.degradeAfter)
		l.enterDegraded()
	}()
}

func (l *Lab) enterDegraded() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isDegraded {
		return
	}
	l.isDegraded = true
	l.degraded.Set(1)
	l.iters.Set(float64(l.degradedIters))
}

func (l *Lab) currentIters() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isDegraded {
		return l.degradedIters
	}
	return l.baselineIters
}

func (l *Lab) degradedMode() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.isDegraded
}

// sink keeps burn's result alive across calls so the compiler cannot remove
// the loop. Tests read it to prove work ran.
var sink atomic.Uint64

// fnvOffset and fnvPrime are FNV-1a 64-bit constants. The loop is integer
// arithmetic the CPU must execute; it is not a sleep and does not write a
// fake duration into Prometheus.
const (
	fnvOffset = 14695981039346656037
	fnvPrime  = 1099511628211
)

func burn(n int) uint64 {
	h := uint64(fnvOffset)
	for i := 1; i <= n; i++ {
		h ^= uint64(i)
		h *= fnvPrime
	}
	sink.Add(h)
	return h
}

// calibrateIters returns an iteration count whose burn duration is near
// target on this machine. It is a starting point for a run, not a benchmark
// result to publish.
func calibrateIters(target time.Duration) int {
	if target <= 0 {
		return 1
	}
	burn(50_000)
	n := 50_000
	for range 12 {
		start := time.Now()
		burn(n)
		d := time.Since(start)
		if d <= 0 {
			n *= 2
			continue
		}
		next := int(float64(n) * float64(target) / float64(d))
		if next < 1 {
			next = 1
		}
		if next > n*8 {
			next = n * 8
		}
		if next > maxIters {
			next = maxIters
		}
		n = next
		delta := d - target
		if delta < 0 {
			delta = -delta
		}
		if delta <= target/5 {
			return n
		}
	}
	if n < 1 {
		return 1
	}
	return n
}
