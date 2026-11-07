package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

// Coordinator runs experiments with a workload runner and a telemetry reader.
//
// Run takes one baseline scrape, starts the workload, scrapes every interval
// while it runs, and takes one final scrape after it returns. Scrapes are made
// one at a time, so they never overlap. A time.Ticker holds at most one
// pending tick: after a scrape longer than the interval, the next scrape
// starts immediately and any further ticks are dropped. Telemetry failures and
// the retention budget never stop the workload: they are recorded and make the
// telemetry partial.
type Coordinator struct {
	Runner workload.Runner
	Reader telemetry.Reader

	// For tests; nil means the real clock and time.Ticker.
	now       func() time.Time
	newTicker func(time.Duration) ticker
}

type ticker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct{ *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.Ticker.C }

// Run validates cfg (after applying defaults) and runs the experiment. It
// returns an error only if cfg is invalid or the coordinator is incomplete;
// every experiment that starts produces a Report, whatever its outcome. All
// goroutines it starts have exited when it returns.
func (c *Coordinator) Run(ctx context.Context, cfg Config) (Report, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return Report{}, err
	}
	if c.Runner == nil || c.Reader == nil {
		return Report{}, errors.New("experiment: a workload runner and a telemetry reader are required")
	}
	now := c.now
	if now == nil {
		now = time.Now
	}
	newTicker := c.newTicker
	if newTicker == nil {
		newTicker = func(d time.Duration) ticker { return realTicker{time.NewTicker(d)} }
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	started := now()
	col := &collector{reader: c.Reader, now: now, start: started, limit: cfg.MaxSamples,
		budget: cfg.MaxTelemetryBytes, metricsURL: cfg.MetricsURL}

	col.scrape(ctx, BeforeWorkload)

	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		col.sampleUntil(ctx, stop, newTicker(cfg.Interval))
	}()
	result, runErr := c.Runner.Run(ctx, cfg.Workload)
	close(stop)
	<-sampled

	if ctx.Err() == nil && !col.budgetReached {
		col.scrape(ctx, AfterWorkload)
	}
	finished := now()

	report := Report{
		SchemaVersion: ReportSchemaVersion,
		StartedAt:     started.UTC(),
		FinishedAt:    finished.UTC(),
		Config: ReportConfig{
			TargetURL:             workload.RedactURL(cfg.Workload.TargetURL),
			VUs:                   cfg.Workload.VUs,
			DurationSeconds:       cfg.Workload.Duration.Seconds(),
			MaxFailureRate:        cfg.MaxFailureRate,
			MetricsURL:            workload.RedactURL(cfg.MetricsURL),
			SampleIntervalSeconds: cfg.Interval.Seconds(),
			MaxSamples:            cfg.MaxSamples,
			MaxTelemetryBytes:     cfg.MaxTelemetryBytes,
			TimeoutSeconds:        cfg.Timeout.Seconds(),
		},
		Telemetry: col.outcome(ctx.Err() != nil),
	}
	if runErr != nil {
		report.Workload = WorkloadOutcome{
			Error:   workload.RedactText(runErr.Error(), cfg.Workload.TargetURL),
			Verdict: workload.Verdict{MaxFailureRate: cfg.MaxFailureRate, Reasons: []string{"the run did not complete"}},
		}
	} else {
		// Taken from the config, so the report never depends on a runner
		// having redacted its URL.
		result.TargetURL = report.Config.TargetURL
		report.Workload = WorkloadOutcome{Result: &result, Verdict: workload.Evaluate(result.Metrics, cfg.MaxFailureRate)}
	}
	report.Status, report.Reasons = judge(ctx.Err(), cfg.Timeout, report.Workload, report.Telemetry)
	return report, nil
}

// judge decides the experiment's status. The order matters: an experiment
// that was stopped or whose workload did not complete is never judged on its
// partial results, and incomplete telemetry is reported only when the
// workload itself passed.
func judge(ctxErr error, timeout time.Duration, w WorkloadOutcome, t TelemetryOutcome) (Status, []string) {
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return StatusTimedOut, []string{fmt.Sprintf("the experiment did not finish before its deadline (timeout %v)", timeout)}
	case ctxErr != nil:
		return StatusCancelled, []string{"the experiment was cancelled before it finished"}
	case w.Result == nil:
		return StatusError, []string{"the workload did not complete: " + w.Error}
	case !w.Verdict.Passed:
		return StatusFailed, append([]string(nil), w.Verdict.Reasons...)
	case t.Completeness != Complete:
		return StatusIncomplete, append([]string{"telemetry is " + string(t.Completeness)}, t.CompletenessReasons...)
	}
	return StatusPassed, nil
}

// collector records scrapes. Only one goroutine uses it at a time: Run before
// and after the sampler, and the sampler in between.
type collector struct {
	reader     telemetry.Reader
	now        func() time.Time
	start      time.Time
	limit      int
	budget     int64
	metricsURL string

	attempts      int
	samples       []Sample
	errs          []ScrapeError
	notRetained   []NotRetained
	retained      int64
	limitReached  bool
	budgetReached bool
}

// sampleUntil scrapes on every tick until stop is closed or ctx is done. It
// keeps one attempt in reserve for the scrape after the workload, and stops
// once the retention budget is reached: further scrapes could not be kept.
func (c *collector) sampleUntil(ctx context.Context, stop <-chan struct{}, t ticker) {
	defer t.Stop()
	for {
		if c.budgetReached {
			return
		}
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C():
		}
		// A tick and stop can be ready together; stopping wins.
		select {
		case <-stop:
			return
		default:
		}
		c.scrape(ctx, DuringWorkload)
		if c.attempts >= c.limit-1 {
			c.limitReached = true
			return
		}
	}
}

func (c *collector) scrape(ctx context.Context, phase Phase) {
	at := c.now()
	snap, err := c.reader.Read(ctx)
	if err != nil && ctx.Err() != nil {
		// Interrupted by the experiment stopping, not an endpoint failure.
		return
	}
	c.attempts++
	if err != nil {
		c.errs = append(c.errs, ScrapeError{
			Sequence:      c.attempts,
			Phase:         phase,
			AttemptedAt:   at.UTC(),
			OffsetSeconds: at.Sub(c.start).Seconds(),
			Error:         workload.RedactText(err.Error(), c.metricsURL),
		})
		return
	}
	s := Sample{
		Sequence:              c.attempts,
		Phase:                 phase,
		ScrapedAt:             snap.ScrapedAt.UTC(),
		OffsetSeconds:         snap.ScrapedAt.Sub(c.start).Seconds(),
		ScrapeDurationSeconds: snap.ScrapeDurationSeconds,
		Families:              snap.Families,
	}
	size, err := sampleSize(s)
	if err != nil {
		c.errs = append(c.errs, ScrapeError{Sequence: s.Sequence, Phase: phase, AttemptedAt: at.UTC(),
			OffsetSeconds: at.Sub(c.start).Seconds(), Error: "measuring the sample: " + err.Error()})
		return
	}
	if c.retained+size > c.budget {
		c.budgetReached = true
		c.notRetained = append(c.notRetained, NotRetained{Sequence: s.Sequence, Phase: phase,
			ScrapedAt: s.ScrapedAt, OffsetSeconds: s.OffsetSeconds, SizeBytes: size})
		return
	}
	c.retained += size
	c.samples = append(c.samples, s)
}

// sampleSize is the size of s in the report, which is what the retention
// budget counts.
func sampleSize(s Sample) (int64, error) {
	data, err := json.Marshal(s)
	return int64(len(data)), err
}

// outcome applies the completeness policy (see Completeness).
func (c *collector) outcome(stopped bool) TelemetryOutcome {
	o := TelemetryOutcome{
		Attempts:              c.attempts,
		Succeeded:             len(c.samples) + len(c.notRetained),
		Failed:                len(c.errs),
		SampleLimitReached:    c.limitReached,
		RetainedBytes:         c.retained,
		RetentionLimitReached: c.budgetReached,
		Samples:               c.samples,
		Errors:                c.errs,
		NotRetained:           c.notRetained,
	}
	if o.Samples == nil {
		o.Samples = []Sample{}
	}
	if o.Errors == nil {
		o.Errors = []ScrapeError{}
	}
	if o.NotRetained == nil {
		o.NotRetained = []NotRetained{}
	}

	var reasons []string
	if o.Failed > 0 {
		reasons = append(reasons, fmt.Sprintf("%d of %d scrapes failed", o.Failed, o.Attempts))
	}
	if o.SampleLimitReached {
		reasons = append(reasons, fmt.Sprintf("sampling stopped at the limit of %d samples while the workload was still running", c.limit))
	}
	if o.RetentionLimitReached {
		n := c.notRetained[0]
		reasons = append(reasons, fmt.Sprintf(
			"telemetry retention budget of %d bytes reached: sample #%d (%d bytes) did not fit after %d bytes were kept; it was not kept and sampling stopped",
			c.budget, n.Sequence, n.SizeBytes, c.retained))
	}
	for _, p := range []Phase{BeforeWorkload, DuringWorkload, AfterWorkload} {
		if !c.hasSample(p) {
			msg := "no sample " + phaseText(p)
			switch {
			case p == AfterWorkload && stopped:
				msg += " (the experiment was stopped)"
			case p == AfterWorkload && c.budgetReached:
				msg += " (the retention budget was reached)"
			}
			reasons = append(reasons, msg)
		}
	}

	switch {
	case len(o.Samples) == 0:
		o.Completeness = None
	case len(reasons) > 0:
		o.Completeness = Partial
	default:
		o.Completeness = Complete
	}
	o.CompletenessReasons = reasons
	return o
}

func (c *collector) hasSample(p Phase) bool {
	for _, s := range c.samples {
		if s.Phase == p {
			return true
		}
	}
	return false
}

func phaseText(p Phase) string {
	switch p {
	case BeforeWorkload:
		return "before the workload"
	case DuringWorkload:
		return "while the workload was running"
	}
	return "after the workload"
}
