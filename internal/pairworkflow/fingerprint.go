package pairworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/diagnosis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/workload"
)

// Fingerprint is the baseline configuration the after stage must match
// before a workload starts. URLs are redacted. TargetSHA256 and
// MetricsSHA256 are consistency fingerprints: SHA-256 of the original URL
// strings, so a different user name, password, or query is rejected without
// storing that secret. They are not a secure substitute for protecting a
// short or guessable secret, because anyone who can guess the URL can test
// it against the hash.
//
// An empty hash means the original string is gone. It is never filled with
// a hash of the redacted URL. An after workload is refused until both
// hashes are present.
type Fingerprint struct {
	TargetURL             string  `json:"target_url"`
	TargetSHA256          string  `json:"target_sha256,omitempty"`
	MetricsURL            string  `json:"metrics_url"`
	MetricsSHA256         string  `json:"metrics_sha256,omitempty"`
	VUs                   int     `json:"vus"`
	DurationSeconds       float64 `json:"duration_seconds"`
	SampleIntervalSeconds float64 `json:"sample_interval_seconds"`
	MaxSamples            int     `json:"max_samples"`
	MaxTelemetryBytes     int64   `json:"max_telemetry_bytes"`
	TimeoutSeconds        float64 `json:"timeout_seconds"`
	MaxFailureRate        float64 `json:"max_failure_rate"`
	RequestsCounter       string  `json:"requests_counter"`
	DurationHistogram     string  `json:"duration_histogram"`
	MethodLabel           string  `json:"method_label"`
	RouteLabel            string  `json:"route_label"`
	StatusLabel           string  `json:"status_label"`
	MinObservations       int     `json:"min_observations"`
}

func fingerprintFromConfig(d diagnosis.Config) Fingerprint {
	d = d.WithDefaults()
	e := d.Experiment
	a := d.Analysis
	return Fingerprint{
		TargetURL:             workload.RedactURL(e.Workload.TargetURL),
		TargetSHA256:          hashText(e.Workload.TargetURL),
		MetricsURL:            workload.RedactURL(e.MetricsURL),
		MetricsSHA256:         hashText(e.MetricsURL),
		VUs:                   e.Workload.VUs,
		DurationSeconds:       e.Workload.Duration.Seconds(),
		SampleIntervalSeconds: e.Interval.Seconds(),
		MaxSamples:            e.MaxSamples,
		MaxTelemetryBytes:     e.MaxTelemetryBytes,
		TimeoutSeconds:        e.Timeout.Seconds(),
		MaxFailureRate:        e.MaxFailureRate,
		RequestsCounter:       a.RequestsCounter,
		DurationHistogram:     a.DurationHistogram,
		MethodLabel:           a.MethodLabel,
		RouteLabel:            a.RouteLabel,
		StatusLabel:           a.StatusLabel,
		MinObservations:       a.MinObservations,
	}
}

// fingerprintFromSaved copies the redacted configuration stored in a
// diagnosis. The original URL strings are not in those files, so the hash
// fields stay empty. Hashing the redacted URL would invent an identity the
// baseline never recorded.
func fingerprintFromSaved(exp experiment.Report, cfg analysis.Config) Fingerprint {
	return Fingerprint{
		TargetURL:             exp.Config.TargetURL,
		MetricsURL:            exp.Config.MetricsURL,
		VUs:                   exp.Config.VUs,
		DurationSeconds:       exp.Config.DurationSeconds,
		SampleIntervalSeconds: exp.Config.SampleIntervalSeconds,
		MaxSamples:            exp.Config.MaxSamples,
		MaxTelemetryBytes:     exp.Config.MaxTelemetryBytes,
		TimeoutSeconds:        exp.Config.TimeoutSeconds,
		MaxFailureRate:        exp.Config.MaxFailureRate,
		RequestsCounter:       cfg.RequestsCounter,
		DurationHistogram:     cfg.DurationHistogram,
		MethodLabel:           cfg.MethodLabel,
		RouteLabel:            cfg.RouteLabel,
		StatusLabel:           cfg.StatusLabel,
		MinObservations:       cfg.MinObservations,
	}
}

// sameRedactedConfig reports whether the stored fingerprint describes the
// saved baseline report. Hashes are not checked here: the saved report no
// longer contains the raw URL.
func sameRedactedConfig(stored Fingerprint, saved Fingerprint) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if stored.TargetURL != saved.TargetURL {
		add("fingerprint target URL does not match before/experiment.json")
	}
	if stored.MetricsURL != saved.MetricsURL {
		add("fingerprint metrics URL does not match before/experiment.json")
	}
	if stored.VUs != saved.VUs {
		add("fingerprint virtual users do not match before/experiment.json")
	}
	if !sameFloat(stored.DurationSeconds, saved.DurationSeconds) || !sameFloat(stored.SampleIntervalSeconds, saved.SampleIntervalSeconds) {
		add("fingerprint duration or interval does not match before/experiment.json")
	}
	if stored.MaxSamples != saved.MaxSamples || stored.MaxTelemetryBytes != saved.MaxTelemetryBytes || !sameFloat(stored.TimeoutSeconds, saved.TimeoutSeconds) || !sameFloat(stored.MaxFailureRate, saved.MaxFailureRate) {
		add("fingerprint sampling or timeout settings do not match before/experiment.json")
	}
	if stored.RequestsCounter != saved.RequestsCounter || stored.DurationHistogram != saved.DurationHistogram {
		add("fingerprint metric families do not match before/analysis.json")
	}
	if stored.MethodLabel != saved.MethodLabel || stored.RouteLabel != saved.RouteLabel || stored.StatusLabel != saved.StatusLabel || stored.MinObservations != saved.MinObservations {
		add("fingerprint labels or min observations do not match before/analysis.json")
	}
	return problems
}

// unverifiedIdentity is returned when the original URL strings cannot be
// checked. The after workload must not start.
const unverifiedIdentity = "The original target and metrics URL fingerprints are not available. Saved diagnosis reports store redacted URLs only, and a SHA-256 is not invented from the redacted text. A query or user name that redacts to the same URL cannot be checked, so no after workload was started. The before/ directory was kept. If after/ is already a completed diagnosis, run -stage compare. To capture a new candidate, run -stage before in a new pair directory so the URL fingerprints are recorded."

// mismatch is why the requested after configuration must not start a workload.
// Messages use redacted URLs only.
func mismatch(saved, requested Fingerprint) []string {
	if saved.TargetSHA256 == "" || saved.MetricsSHA256 == "" {
		return []string{unverifiedIdentity}
	}
	var problems []string
	add := func(text string) {
		if text != "" {
			problems = append(problems, text)
		}
	}
	if saved.TargetURL != requested.TargetURL {
		add(fmt.Sprintf("target URL differs (%s before, %s requested)", saved.TargetURL, requested.TargetURL))
	} else if saved.TargetSHA256 != "" && requested.TargetSHA256 != "" && saved.TargetSHA256 != requested.TargetSHA256 {
		add("target URL user information or query differs from the baseline; the redacted URLs match, and the raw URL was not stored")
	}
	if saved.MetricsURL != requested.MetricsURL {
		add(fmt.Sprintf("metrics URL differs (%s before, %s requested)", saved.MetricsURL, requested.MetricsURL))
	} else if saved.MetricsSHA256 != "" && requested.MetricsSHA256 != "" && saved.MetricsSHA256 != requested.MetricsSHA256 {
		add("metrics URL user information or query differs from the baseline; the redacted URLs match, and the raw URL was not stored")
	}
	if saved.VUs != requested.VUs {
		add(fmt.Sprintf("virtual users differ (%d before, %d requested)", saved.VUs, requested.VUs))
	}
	if !sameFloat(saved.DurationSeconds, requested.DurationSeconds) {
		add(fmt.Sprintf("workload duration differs (%.6gs before, %.6gs requested)", saved.DurationSeconds, requested.DurationSeconds))
	}
	if !sameFloat(saved.SampleIntervalSeconds, requested.SampleIntervalSeconds) {
		add(fmt.Sprintf("scrape interval differs (%.6gs before, %.6gs requested)", saved.SampleIntervalSeconds, requested.SampleIntervalSeconds))
	}
	if saved.MaxSamples != requested.MaxSamples {
		add(fmt.Sprintf("max samples differ (%d before, %d requested)", saved.MaxSamples, requested.MaxSamples))
	}
	if saved.MaxTelemetryBytes != requested.MaxTelemetryBytes {
		add(fmt.Sprintf("max telemetry bytes differ (%d before, %d requested)", saved.MaxTelemetryBytes, requested.MaxTelemetryBytes))
	}
	if !sameFloat(saved.TimeoutSeconds, requested.TimeoutSeconds) {
		add(fmt.Sprintf("timeout differs (%.6gs before, %.6gs requested)", saved.TimeoutSeconds, requested.TimeoutSeconds))
	}
	if !sameFloat(saved.MaxFailureRate, requested.MaxFailureRate) {
		add(fmt.Sprintf("max failure rate differs (%.6g before, %.6g requested)", saved.MaxFailureRate, requested.MaxFailureRate))
	}
	if saved.RequestsCounter != requested.RequestsCounter || saved.DurationHistogram != requested.DurationHistogram {
		add(fmt.Sprintf("metric families differ (requests %q / %q, histogram %q / %q)",
			saved.RequestsCounter, requested.RequestsCounter, saved.DurationHistogram, requested.DurationHistogram))
	}
	if saved.MethodLabel != requested.MethodLabel || saved.RouteLabel != requested.RouteLabel || saved.StatusLabel != requested.StatusLabel {
		add(fmt.Sprintf("metric labels differ (method %q/%q, route %q/%q, status %q/%q)",
			saved.MethodLabel, requested.MethodLabel, saved.RouteLabel, requested.RouteLabel, saved.StatusLabel, requested.StatusLabel))
	}
	if saved.MinObservations != requested.MinObservations {
		add(fmt.Sprintf("minimum histogram observations differ (%d before, %d requested)", saved.MinObservations, requested.MinObservations))
	}
	return problems
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sameFloat(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return a == b
	}
	diff := math.Abs(a - b)
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return diff <= 1e-9*scale
}

func joined(problems []string) string {
	return strings.Join(problems, "; ")
}
