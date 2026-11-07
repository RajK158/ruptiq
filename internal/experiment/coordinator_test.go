package experiment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

func phases(samples []Sample) []Phase {
	var out []Phase
	for _, s := range samples {
		out = append(out, s.Phase)
	}
	return out
}

func sequences[T any](items []T, seq func(T) int) []int {
	var out []int
	for _, it := range items {
		out = append(out, seq(it))
	}
	return out
}

func sampleSeq(s Sample) int     { return s.Sequence }
func errorSeq(e ScrapeError) int { return e.Sequence }

func wantReason(t *testing.T, reasons []string, part string) {
	t.Helper()
	for _, r := range reasons {
		if strings.Contains(r, part) {
			return
		}
	}
	t.Errorf("reasons %q do not mention %q", reasons, part)
}

func TestSuccessfulExperiment(t *testing.T) {
	h := newHarness()
	var gotSpec workload.Spec
	report := h.run(t, context.Background(), baseConfig(), func(ctx context.Context, spec workload.Spec) (workload.Result, error) {
		gotSpec = spec
		h.awaitBaseline(t)
		for range 3 {
			h.tick()
		}
		h.clock.Advance(500 * time.Millisecond)
		return healthyResult(), nil
	})

	if report.Status != StatusPassed || report.Reasons != nil {
		t.Errorf("status = %s %q, want passed with no reasons", report.Status, report.Reasons)
	}
	if gotSpec != baseConfig().Workload {
		t.Errorf("runner got spec %+v, want %+v", gotSpec, baseConfig().Workload)
	}
	tel := report.Telemetry
	if tel.Completeness != Complete || tel.CompletenessReasons != nil {
		t.Errorf("completeness = %s %q, want complete", tel.Completeness, tel.CompletenessReasons)
	}
	if tel.Attempts != 5 || tel.Succeeded != 5 || tel.Failed != 0 || tel.SampleLimitReached {
		t.Errorf("counts = %d attempts, %d ok, %d failed, limit %v; want 5, 5, 0, false",
			tel.Attempts, tel.Succeeded, tel.Failed, tel.SampleLimitReached)
	}
	wantPhases := []Phase{BeforeWorkload, DuringWorkload, DuringWorkload, DuringWorkload, AfterWorkload}
	if got := phases(tel.Samples); !slices.Equal(got, wantPhases) {
		t.Errorf("phases = %v, want %v", got, wantPhases)
	}
	if got := sequences(tel.Samples, sampleSeq); !slices.Equal(got, []int{1, 2, 3, 4, 5}) {
		t.Errorf("sequences = %v, want 1 to 5", got)
	}
	wantOffsets := []float64{0, 1, 2, 3, 3.5}
	for i, s := range tel.Samples {
		if s.OffsetSeconds != wantOffsets[i] || !s.ScrapedAt.Equal(t0.Add(time.Duration(wantOffsets[i]*float64(time.Second)))) {
			t.Errorf("sample %d at %v (offset %v), want offset %v", i, s.ScrapedAt, s.OffsetSeconds, wantOffsets[i])
		}
		// The structured families are kept as read: call n exposes n.
		if v := s.Families[0].Series[0].Value; v == nil || float64(*v) != float64(i+1) {
			t.Errorf("sample %d counter = %v, want %d", i, v, i+1)
		}
	}
	if !report.StartedAt.Equal(t0) || !report.FinishedAt.Equal(t0.Add(3500*time.Millisecond)) {
		t.Errorf("experiment ran %v to %v", report.StartedAt, report.FinishedAt)
	}
	if w := report.Workload; w.Result == nil || w.Error != "" || !w.Verdict.Passed {
		t.Errorf("workload = %+v, want a passed result", w)
	}
	if got := h.reader.callCount(); got != 5 {
		t.Errorf("reader called %d times, want 5", got)
	}
}

func TestEarlyWorkloadCompletion(t *testing.T) {
	h := newHarness()
	report := h.run(t, context.Background(), baseConfig(), func(context.Context, workload.Spec) (workload.Result, error) {
		return healthyResult(), nil
	})
	tel := report.Telemetry
	if got := phases(tel.Samples); !slices.Equal(got, []Phase{BeforeWorkload, AfterWorkload}) {
		t.Errorf("phases = %v, want before and after only", got)
	}
	if tel.Completeness != Partial {
		t.Errorf("completeness = %s, want partial", tel.Completeness)
	}
	wantReason(t, tel.CompletenessReasons, "no sample while the workload was running")
	if report.Status != StatusIncomplete {
		t.Errorf("status = %s, want incomplete: a passed workload without telemetry during it is not a pass", report.Status)
	}
}

func TestSamplingStopsWhenWorkloadReturns(t *testing.T) {
	h := newHarness()
	report := h.run(t, context.Background(), baseConfig(), func(context.Context, workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		h.tick()
		return healthyResult(), nil
	})
	calls := h.reader.callCount()
	if calls != 3 || report.Telemetry.Attempts != 3 {
		t.Fatalf("reader called %d times, %d attempts recorded; want 3 (before, during, after)", calls, report.Telemetry.Attempts)
	}
	// The sampler has exited: nobody receives a tick any more.
	select {
	case h.ticker.ch <- time.Now():
		t.Fatal("the sampler still receives ticks after the experiment ended")
	default:
	}
	if got := h.reader.callCount(); got != calls {
		t.Errorf("reader called %d times after the experiment ended", got-calls)
	}
}

// TestRealTickerSampling runs with the real clock and ticker: scrapes happen
// while the workload runs, never overlap, and stop when it returns.
func TestRealTickerSampling(t *testing.T) {
	clock := newFakeClock()
	reader := newFakeReader(clock)
	slowReads := &slowReader{fakeReader: reader, delay: 30 * time.Millisecond}
	cfg := baseConfig()
	cfg.Interval = MinInterval
	c := &Coordinator{Reader: slowReads, Runner: runnerFunc(func(ctx context.Context, _ workload.Spec) (workload.Result, error) {
		for done := 0; done < 4; {
			select {
			case <-reader.done:
				done++
			case <-ctx.Done():
				return workload.Result{}, ctx.Err()
			}
		}
		return healthyResult(), nil
	})}
	report, err := c.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusPassed {
		t.Fatalf("status = %s %q, want passed", report.Status, report.Reasons)
	}
	if got := reader.maxIn.Load(); got != 1 {
		t.Errorf("at most %d scrapes in flight, want 1", got)
	}
	calls := reader.callCount()
	time.Sleep(3 * MinInterval)
	if got := reader.callCount(); got != calls {
		t.Errorf("%d scrapes after the experiment ended", got-calls)
	}
	for i := 1; i < len(report.Telemetry.Samples); i++ {
		a, b := report.Telemetry.Samples[i-1], report.Telemetry.Samples[i]
		if b.Sequence <= a.Sequence {
			t.Errorf("samples out of order: #%d before #%d", a.Sequence, b.Sequence)
		}
	}
}

type slowReader struct {
	*fakeReader
	delay time.Duration
}

func (r *slowReader) Read(ctx context.Context) (telemetry.Snapshot, error) {
	time.Sleep(r.delay)
	return r.fakeReader.Read(ctx)
}

func TestNoOverlappingScrapes(t *testing.T) {
	h := newHarness()
	h.reader.block[2] = true
	var dropped atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report := h.run(t, ctx, baseConfig(), func(ctx context.Context, _ workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		h.ticker.ch <- h.clock.Now()
		<-h.reader.started // baseline
		<-h.reader.started // scrape 2 is now in progress and blocked
		for range 5 {
			select {
			case h.ticker.ch <- h.clock.Now():
				t.Error("a tick was accepted while a scrape was in progress")
			default:
				dropped.Add(1)
			}
		}
		cancel()
		return workload.Result{}, fmt.Errorf("k6 run stopped: %w", ctx.Err())
	})
	if dropped.Load() != 5 {
		t.Errorf("%d ticks dropped, want 5", dropped.Load())
	}
	if report.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled", report.Status)
	}
}

func TestCancellation(t *testing.T) {
	h := newHarness()
	h.reader.block[3] = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report := h.run(t, ctx, baseConfig(), func(ctx context.Context, _ workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		h.tick()
		h.ticker.ch <- h.clock.Now()
		for n := range h.reader.started {
			if n == 3 {
				break
			}
		}
		cancel() // interrupts scrape 3 and the workload
		<-ctx.Done()
		return workload.Result{}, fmt.Errorf("k6 run stopped: %w", ctx.Err())
	})

	if report.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled", report.Status)
	}
	wantReason(t, report.Reasons, "cancelled")
	tel := report.Telemetry
	// The interrupted scrape is not an endpoint failure, and no scrape is
	// made after a cancelled workload.
	if tel.Attempts != 2 || tel.Failed != 0 || h.reader.callCount() != 3 {
		t.Errorf("%d attempts, %d failed, %d reads; want 2, 0, 3", tel.Attempts, tel.Failed, h.reader.callCount())
	}
	if tel.Completeness != Partial {
		t.Errorf("completeness = %s, want partial", tel.Completeness)
	}
	wantReason(t, tel.CompletenessReasons, "no sample after the workload (the experiment was stopped)")
	if report.Workload.Result != nil || !strings.Contains(report.Workload.Error, "context canceled") || report.Workload.Verdict.Passed {
		t.Errorf("workload = %+v, want the cancellation error and no pass", report.Workload)
	}
}

func TestTimeout(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	report := h.run(t, ctx, baseConfig(), func(ctx context.Context, _ workload.Spec) (workload.Result, error) {
		<-ctx.Done()
		return workload.Result{}, fmt.Errorf("k6 run stopped: %w", ctx.Err())
	})
	if report.Status != StatusTimedOut {
		t.Errorf("status = %s, want timed_out", report.Status)
	}
	wantReason(t, report.Reasons, "deadline")
}

func TestTimeoutConfig(t *testing.T) {
	cfg := baseConfig()
	cfg.Timeout = 7 * time.Second
	var remaining time.Duration
	h := newHarness()
	report := h.run(t, context.Background(), cfg, func(ctx context.Context, _ workload.Spec) (workload.Result, error) {
		d, _ := ctx.Deadline()
		remaining = time.Until(d)
		return healthyResult(), nil
	})
	if remaining <= 6*time.Second || remaining > 7*time.Second {
		t.Errorf("workload deadline in %v, want about 7s", remaining)
	}
	if report.Config.TimeoutSeconds != 7 {
		t.Errorf("recorded timeout %v, want 7", report.Config.TimeoutSeconds)
	}

	h = newHarness()
	report = h.run(t, context.Background(), baseConfig(), func(context.Context, workload.Spec) (workload.Result, error) {
		return healthyResult(), nil
	})
	if report.Config.TimeoutSeconds != 65 || report.Config.MaxSamples != DefaultMaxSamples {
		t.Errorf("defaults: timeout %v, max samples %d; want 65 and %d",
			report.Config.TimeoutSeconds, report.Config.MaxSamples, DefaultMaxSamples)
	}
}

func TestWorkloadError(t *testing.T) {
	h := newHarness()
	report := h.run(t, context.Background(), baseConfig(), func(context.Context, workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		h.tick()
		return workload.Result{}, errors.New("k6 exited with status 107: script error")
	})
	if report.Status != StatusError {
		t.Errorf("status = %s, want error", report.Status)
	}
	wantReason(t, report.Reasons, "status 107")
	w := report.Workload
	if w.Result != nil || w.Error != "k6 exited with status 107: script error" || w.Verdict.Passed {
		t.Errorf("workload = %+v", w)
	}
	// The telemetry around a failed workload is still kept.
	if report.Telemetry.Completeness != Complete || report.Telemetry.Succeeded != 3 {
		t.Errorf("telemetry %s with %d samples, want complete with 3", report.Telemetry.Completeness, report.Telemetry.Succeeded)
	}
}

func TestWorkloadWithHTTPFailures(t *testing.T) {
	h := newHarness()
	report := h.run(t, context.Background(), baseConfig(), func(context.Context, workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		h.tick()
		r := healthyResult()
		r.Metrics.FailedRequests, r.Metrics.FailureRate = 5000, 0.25
		return r, nil
	})
	if report.Status != StatusFailed {
		t.Errorf("status = %s, want failed", report.Status)
	}
	wantReason(t, report.Reasons, "failure rate 25.00% exceeds the allowed 0.00%")
	if report.Workload.Result == nil || report.Workload.Verdict.Passed {
		t.Errorf("workload = %+v, want a failed result", report.Workload)
	}
}

func TestTelemetryUnavailable(t *testing.T) {
	h := newHarness()
	for n := 1; n <= 10; n++ {
		h.reader.fail[n] = errRefused
	}
	report := h.run(t, context.Background(), baseConfig(), func(context.Context, workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		h.tick()
		h.tick()
		return healthyResult(), nil
	})
	tel := report.Telemetry
	if tel.Completeness != None || tel.Succeeded != 0 || tel.Failed != 4 || len(tel.Samples) != 0 {
		t.Errorf("telemetry = %s, %d ok, %d failed, %d samples; want none, 0, 4, 0",
			tel.Completeness, tel.Succeeded, tel.Failed, len(tel.Samples))
	}
	if got := sequences(tel.Errors, errorSeq); !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Errorf("error sequences = %v", got)
	}
	if e := tel.Errors[0]; e.Phase != BeforeWorkload || e.Error != errRefused.Error() || !e.AttemptedAt.Equal(t0) {
		t.Errorf("first error = %+v", e)
	}
	if report.Status != StatusIncomplete {
		t.Errorf("status = %s, want incomplete", report.Status)
	}
	wantReason(t, report.Reasons, "telemetry is none")
	wantReason(t, report.Reasons, "4 of 4 scrapes failed")
}

func TestIntermittentScrapeFailures(t *testing.T) {
	h := newHarness()
	h.reader.fail[2] = errRefused
	h.reader.fail[4] = errors.New("metrics endpoint returned an error status: GET http://127.0.0.1:8082/metrics: 503 Service Unavailable")
	report := h.run(t, context.Background(), baseConfig(), func(context.Context, workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		for range 3 {
			h.tick()
		}
		return healthyResult(), nil
	})
	tel := report.Telemetry
	if got := sequences(tel.Samples, sampleSeq); !slices.Equal(got, []int{1, 3, 5}) {
		t.Errorf("sample sequences = %v, want [1 3 5]", got)
	}
	if got := sequences(tel.Errors, errorSeq); !slices.Equal(got, []int{2, 4}) {
		t.Errorf("error sequences = %v, want [2 4]", got)
	}
	if tel.Attempts != 5 || tel.Completeness != Partial {
		t.Errorf("%d attempts, %s; want 5, partial", tel.Attempts, tel.Completeness)
	}
	wantReason(t, tel.CompletenessReasons, "2 of 5 scrapes failed")
	if report.Status != StatusIncomplete {
		t.Errorf("status = %s, want incomplete", report.Status)
	}
}

func TestSampleLimit(t *testing.T) {
	cfg := baseConfig()
	cfg.Workload.Duration = time.Second
	cfg.MaxSamples = 3
	h := newHarness()
	report := h.run(t, context.Background(), cfg, func(context.Context, workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		h.tick()
		<-h.ticker.stopped // the sampler gave up while the workload still runs
		h.clock.Advance(5 * time.Second)
		return healthyResult(), nil
	})
	tel := report.Telemetry
	if !tel.SampleLimitReached || tel.Attempts != 3 {
		t.Errorf("limit reached %v after %d attempts, want true after 3", tel.SampleLimitReached, tel.Attempts)
	}
	if got := phases(tel.Samples); !slices.Equal(got, []Phase{BeforeWorkload, DuringWorkload, AfterWorkload}) {
		t.Errorf("phases = %v; the last slot is kept for the scrape after the workload", got)
	}
	if tel.Completeness != Partial {
		t.Errorf("completeness = %s, want partial", tel.Completeness)
	}
	wantReason(t, tel.CompletenessReasons, "limit of 3 samples")
	if report.Status != StatusIncomplete {
		t.Errorf("status = %s, want incomplete", report.Status)
	}
}

func TestRetentionBudgetReached(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxTelemetryBytes = 4096
	h := newHarness()
	h.reader.pad[2] = 1000
	h.reader.pad[3] = 3000 // fits on its own, but not after samples 1 and 2
	report := h.run(t, context.Background(), cfg, func(context.Context, workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		h.tick()
		h.tick()
		<-h.ticker.stopped // sampling stopped at the budget; the workload carries on
		h.clock.Advance(3 * time.Second)
		return healthyResult(), nil
	})

	tel := report.Telemetry
	if got := sequences(tel.Samples, sampleSeq); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("kept samples %v, want [1 2]: samples kept before the budget was reached stay", got)
	}
	if len(tel.NotRetained) != 1 {
		t.Fatalf("not retained = %+v, want sample 3 only", tel.NotRetained)
	}
	n := tel.NotRetained[0]
	if n.Sequence != 3 || n.Phase != DuringWorkload || !n.ScrapedAt.Equal(t0.Add(2*time.Second)) {
		t.Errorf("not retained = %+v", n)
	}
	if n.SizeBytes >= cfg.MaxTelemetryBytes || tel.RetainedBytes+n.SizeBytes <= cfg.MaxTelemetryBytes {
		t.Errorf("sample 3 has %d bytes after %d kept; the test needs it to fit alone but not in what is left of %d",
			n.SizeBytes, tel.RetainedBytes, cfg.MaxTelemetryBytes)
	}
	if !tel.RetentionLimitReached || tel.RetainedBytes > cfg.MaxTelemetryBytes || tel.Attempts != 3 || tel.Succeeded != 3 {
		t.Errorf("limit reached %v, %d bytes kept, %d attempts, %d succeeded", tel.RetentionLimitReached, tel.RetainedBytes, tel.Attempts, tel.Succeeded)
	}
	if got := h.reader.callCount(); got != 3 {
		t.Errorf("reader called %d times, want 3: no scrapes after the budget is reached, not even after the workload", got)
	}
	if tel.Completeness != Partial {
		t.Errorf("completeness = %s, want partial", tel.Completeness)
	}
	wantReason(t, tel.CompletenessReasons, fmt.Sprintf("retention budget of 4096 bytes reached: sample #3 (%d bytes) did not fit after %d bytes were kept",
		n.SizeBytes, tel.RetainedBytes))
	wantReason(t, tel.CompletenessReasons, "no sample after the workload (the retention budget was reached)")
	if report.Status != StatusIncomplete {
		t.Errorf("status = %s, want incomplete", report.Status)
	}
	if w := report.Workload; w.Result == nil || !w.Verdict.Passed {
		t.Errorf("workload = %+v, want its passed result untouched by the budget", w)
	}
}

func TestSnapshotLargerThanWholeBudget(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxTelemetryBytes = 4096
	h := newHarness()
	h.reader.pad[1] = 10000
	report := h.run(t, context.Background(), cfg, func(context.Context, workload.Spec) (workload.Result, error) {
		<-h.ticker.stopped
		return healthyResult(), nil
	})

	tel := report.Telemetry
	if len(tel.Samples) != 0 || tel.RetainedBytes != 0 || len(tel.NotRetained) != 1 || tel.NotRetained[0].SizeBytes <= 10000 {
		t.Errorf("%d samples, %d bytes kept, not retained %+v; want nothing kept and the baseline (over 10000 bytes) listed",
			len(tel.Samples), tel.RetainedBytes, tel.NotRetained)
	}
	if tel.Completeness != None || report.Status != StatusIncomplete {
		t.Errorf("completeness %s, status %s; want none, incomplete", tel.Completeness, report.Status)
	}
	if got := h.reader.callCount(); got != 1 {
		t.Errorf("reader called %d times, want 1", got)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("xxxxxxxxxx")) || len(data) > 4096 {
		t.Errorf("the report (%d bytes) contains the oversized sample's data", len(data))
	}
}

func TestDefaultRetentionBudget(t *testing.T) {
	h := newHarness()
	report := h.run(t, context.Background(), baseConfig(), func(context.Context, workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		h.tick()
		return healthyResult(), nil
	})
	if report.Config.MaxTelemetryBytes != 32<<20 {
		t.Errorf("default budget %d, want 32 MiB", report.Config.MaxTelemetryBytes)
	}
	if tel := report.Telemetry; tel.RetentionLimitReached || tel.RetainedBytes <= 0 || len(tel.NotRetained) != 0 {
		t.Errorf("limit reached %v, %d bytes kept, %d not retained", tel.RetentionLimitReached, tel.RetainedBytes, len(tel.NotRetained))
	}
}

func TestInvalidConfig(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
		want   []string
	}{
		{"interval too short", func(c *Config) { c.Interval = time.Millisecond }, []string{"interval must be between"}},
		{"interval longer than workload", func(c *Config) { c.Interval = 10 * time.Second }, []string{"must not be longer than the workload"}},
		{"too many samples", func(c *Config) { c.MaxSamples = 4 }, []string{"needs about 7 samples, more than max samples (4)"}},
		{"max samples out of range", func(c *Config) { c.MaxSamples = MaxSamplesLimit + 1 }, []string{"max samples must be between"}},
		{"timeout too short", func(c *Config) { c.Timeout = 5 * time.Second }, []string{"timeout (5s) must be longer"}},
		{"metrics URL", func(c *Config) { c.MetricsURL = "ftp://u:hunter2@host/metrics?k=v" }, []string{"metrics URL: invalid metrics endpoint"}},
		{"missing metrics URL", func(c *Config) { c.MetricsURL = "" }, []string{"URL is required"}},
		{"failure rate", func(c *Config) { c.MaxFailureRate = 2 }, []string{"max failure rate"}},
		{"budget too small", func(c *Config) { c.MaxTelemetryBytes = 100 }, []string{"max telemetry bytes must be between 1024 and 1073741824, got 100"}},
		{"budget too large", func(c *Config) { c.MaxTelemetryBytes = 2 << 30 }, []string{"max telemetry bytes must be between"}},
		{"several", func(c *Config) { c.Workload.VUs = 0; c.Interval = -time.Second }, []string{"vus must be", "interval must be"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			tt.change(&cfg)
			called := false
			c := &Coordinator{Reader: newFakeReader(newFakeClock()), Runner: runnerFunc(func(context.Context, workload.Spec) (workload.Result, error) {
				called = true
				return healthyResult(), nil
			})}
			_, err := c.Run(context.Background(), cfg)
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error = %v, want a ValidationError", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error shows the password: %v", err)
			}
			if called {
				t.Error("the workload ran despite an invalid config")
			}
		})
	}

	if _, err := (&Coordinator{}).Run(context.Background(), baseConfig()); err == nil {
		t.Error("a coordinator without runner and reader was accepted")
	}
}
