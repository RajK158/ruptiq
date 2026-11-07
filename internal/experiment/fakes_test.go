package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// fakeClock is a clock that moves only when told to.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: t0} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// manualTicker ticks only when a test sends on ch. ch is unbuffered, so a send
// completes only when the sampler is waiting for a tick.
type manualTicker struct {
	ch       chan time.Time
	stopped  chan struct{}
	stopOnce sync.Once
}

func newManualTicker() *manualTicker {
	return &manualTicker{ch: make(chan time.Time), stopped: make(chan struct{})}
}

func (t *manualTicker) C() <-chan time.Time { return t.ch }
func (t *manualTicker) Stop()               { t.stopOnce.Do(func() { close(t.stopped) }) }

func (t *manualTicker) isStopped() bool {
	select {
	case <-t.stopped:
		return true
	default:
		return false
	}
}

// fakeReader returns a snapshot or a scripted error for each call. Call n
// (1-based) fails with fail[n] if set and blocks until its context is done
// if block[n] is set. Each snapshot has one counter whose value is the call
// number, so values increase with every successful scrape.
type fakeReader struct {
	clock *fakeClock
	fail  map[int]error
	block map[int]bool
	// pad[n] adds a series to call n's snapshot with a label value of that
	// many bytes, making the snapshot about that much larger.
	pad map[int]int

	mu       sync.Mutex
	calls    int
	inFlight atomic.Int32
	maxIn    atomic.Int32
	started  chan int // receives n when call n starts
	done     chan int // receives n when call n returns
}

func newFakeReader(clock *fakeClock) *fakeReader {
	return &fakeReader{clock: clock, fail: map[int]error{}, block: map[int]bool{}, pad: map[int]int{},
		started: make(chan int, 100), done: make(chan int, 100)}
}

func (r *fakeReader) Read(ctx context.Context) (telemetry.Snapshot, error) {
	r.mu.Lock()
	r.calls++
	n := r.calls
	r.mu.Unlock()

	in := r.inFlight.Add(1)
	for {
		m := r.maxIn.Load()
		if in <= m || r.maxIn.CompareAndSwap(m, in) {
			break
		}
	}
	defer func() {
		r.inFlight.Add(-1)
		r.done <- n
	}()
	r.started <- n

	if r.block[n] {
		<-ctx.Done()
		return telemetry.Snapshot{}, ctx.Err()
	}
	if err := r.fail[n]; err != nil {
		return telemetry.Snapshot{}, err
	}
	v := telemetry.Float(n)
	series := []telemetry.Series{{Labels: map[string]string{"code": "200"}, Value: &v}}
	if size := r.pad[n]; size > 0 {
		series = append(series, telemetry.Series{Labels: map[string]string{"pad": strings.Repeat("x", size)}, Value: &v})
	}
	return telemetry.Snapshot{
		Endpoint:              "http://127.0.0.1:8082/metrics",
		ScrapedAt:             r.clock.Now(),
		ScrapeDurationSeconds: 0.002,
		Families: []telemetry.Family{{
			Name: "app_requests_total", Help: "Requests handled.", Type: telemetry.Counter,
			Series: series,
		}},
	}, nil
}

func (r *fakeReader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// runnerFunc adapts a function to workload.Runner.
type runnerFunc func(ctx context.Context, spec workload.Spec) (workload.Result, error)

func (f runnerFunc) Run(ctx context.Context, spec workload.Spec) (workload.Result, error) {
	return f(ctx, spec)
}

func healthyResult() workload.Result {
	return workload.Result{
		Runner: "k6", RunnerVersion: "2.3.0",
		TargetURL: "http://127.0.0.1:8082/inventory/LAPTOP-001",
		VUs:       2, ConfiguredDurationSeconds: 5,
		StartedAt:  t0.Add(10 * time.Millisecond),
		FinishedAt: t0.Add(3 * time.Second),
		Metrics: workload.Metrics{
			DurationSeconds: 5.0004, HTTPRequests: 20000, RequestsPerSecond: 20000 / 5.0004,
			LatencyMs: workload.Latency{Avg: 0.42, Min: 0.1, P50: 0.53, P90: 0.62, P95: 0.68, Max: 8.57},
		},
	}
}

func baseConfig() Config {
	return Config{
		Workload:   workload.Spec{TargetURL: "http://127.0.0.1:8082/inventory/LAPTOP-001", VUs: 2, Duration: 5 * time.Second},
		MetricsURL: "http://127.0.0.1:8082/metrics",
		Interval:   time.Second,
	}
}

// harness wires a coordinator to a fake clock, ticker, and reader.
type harness struct {
	clock  *fakeClock
	ticker *manualTicker
	reader *fakeReader
}

func newHarness() *harness {
	clock := newFakeClock()
	return &harness{clock: clock, ticker: newManualTicker(), reader: newFakeReader(clock)}
}

func (h *harness) coordinator(run runnerFunc) *Coordinator {
	return &Coordinator{
		Runner:    run,
		Reader:    h.reader,
		now:       h.clock.Now,
		newTicker: func(time.Duration) ticker { return h.ticker },
	}
}

// awaitBaseline waits for the scrape made before the workload started.
func (h *harness) awaitBaseline(t *testing.T) {
	t.Helper()
	if n := <-h.reader.done; n != 1 {
		t.Fatalf("first completed scrape is call %d, want 1", n)
	}
}

// tick advances the clock by a second, delivers one tick, and waits for the
// scrape it triggers to finish.
func (h *harness) tick() {
	h.clock.Advance(time.Second)
	h.ticker.ch <- h.clock.Now()
	<-h.reader.done
}

func (h *harness) run(t *testing.T, ctx context.Context, cfg Config, run runnerFunc) Report {
	t.Helper()
	report, err := h.coordinator(run).Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !h.ticker.isStopped() {
		t.Error("the sampling ticker was not stopped")
	}
	if got := h.reader.maxIn.Load(); got > 1 {
		t.Errorf("%d scrapes overlapped", got)
	}
	checkAccounting(t, report.Telemetry)
	return report
}

// checkAccounting checks the invariants documented on TelemetryOutcome: every
// attempt appears exactly once in samples, errors, or not_retained; the counts
// agree; and retained_bytes is the JSON size of the kept samples.
func checkAccounting(t *testing.T, o TelemetryOutcome) {
	t.Helper()
	seen := map[int]int{}
	for _, s := range o.Samples {
		seen[s.Sequence]++
	}
	for _, e := range o.Errors {
		seen[e.Sequence]++
	}
	for _, n := range o.NotRetained {
		seen[n.Sequence]++
	}
	for seq := 1; seq <= o.Attempts; seq++ {
		if seen[seq] != 1 {
			t.Errorf("attempt #%d appears %d times", seq, seen[seq])
		}
	}
	if len(seen) != o.Attempts {
		t.Errorf("%d sequence numbers recorded for %d attempts", len(seen), o.Attempts)
	}
	if o.Succeeded != len(o.Samples)+len(o.NotRetained) || o.Failed != len(o.Errors) || o.Attempts != o.Succeeded+o.Failed {
		t.Errorf("counts disagree: %d attempts, %d succeeded, %d failed; %d samples, %d errors, %d not retained",
			o.Attempts, o.Succeeded, o.Failed, len(o.Samples), len(o.Errors), len(o.NotRetained))
	}
	var bytes int64
	for _, s := range o.Samples {
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		bytes += int64(len(data))
	}
	if o.RetainedBytes != bytes {
		t.Errorf("retained_bytes = %d, want %d (the JSON size of the kept samples)", o.RetainedBytes, bytes)
	}
	if o.RetentionLimitReached != (len(o.NotRetained) > 0) {
		t.Errorf("retention_limit_reached = %v with %d samples not retained", o.RetentionLimitReached, len(o.NotRetained))
	}
}

var errRefused = errors.New("metrics request failed: GET http://127.0.0.1:8082/metrics: connection refused")
