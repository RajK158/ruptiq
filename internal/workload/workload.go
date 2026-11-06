// Package workload is Ruptiq Core's WorkloadRunner boundary: what a load run
// is, how it is validated, and what a run reports back.
//
// It knows nothing about any target application or about how load is
// generated; implementations such as package k6 do the actual work.
package workload

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Defaults and limits for a Spec. The defaults are deliberately small so that
// an accidental run is gentle on the target.
const (
	DefaultVUs      = 2
	DefaultDuration = 5 * time.Second

	MaxVUs          = 1000
	MinDuration     = 1 * time.Second
	MaxDuration     = 1 * time.Hour
	MaxTargetURLLen = 2048
)

// Spec describes one HTTP GET load run.
type Spec struct {
	// TargetURL is the absolute http or https URL every virtual user requests.
	TargetURL string
	// VUs is the number of concurrent virtual users.
	VUs int
	// Duration is how long the load runs, in whole seconds.
	Duration time.Duration
}

// ValidationError lists every problem found in a Spec.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid workload: " + strings.Join(e.Problems, "; ")
}

// Validate reports every problem with s, or nil. It never contacts the target.
func (s Spec) Validate() error {
	var problems []string
	if err := validateTargetURL(s.TargetURL); err != nil {
		problems = append(problems, err.Error())
	}
	if s.VUs < 1 || s.VUs > MaxVUs {
		problems = append(problems, fmt.Sprintf("vus must be between 1 and %d, got %d", MaxVUs, s.VUs))
	}
	switch {
	case s.Duration < MinDuration || s.Duration > MaxDuration:
		problems = append(problems, fmt.Sprintf("duration must be between %v and %v, got %v", MinDuration, MaxDuration, s.Duration))
	case s.Duration%time.Second != 0:
		problems = append(problems, fmt.Sprintf("duration must be a whole number of seconds, got %v", s.Duration))
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func validateTargetURL(raw string) error {
	if raw == "" {
		return errors.New("target URL is required")
	}
	if len(raw) > MaxTargetURLLen {
		return fmt.Errorf("target URL must be at most %d characters", MaxTargetURLLen)
	}
	if strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("target URL must not contain spaces or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("target URL is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("target URL must start with http:// or https://")
	}
	if u.Hostname() == "" {
		return errors.New("target URL must include a host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return errors.New("target URL port must be between 1 and 65535")
		}
	}
	if u.Fragment != "" {
		return errors.New("target URL must not contain a fragment")
	}
	return nil
}

// RedactURL returns raw for display and storage: the user information (user
// name and password) and the whole query string are each replaced by
// "xxxxx", because any of them can carry a secret such as an API key. Invalid
// URLs are replaced entirely rather than echoed.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	if u.User != nil {
		u.User = url.User(redacted)
	}
	if u.RawQuery != "" {
		u.RawQuery = redacted
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// RedactText removes the secret parts of targetURL (user information and
// query string) from text, such as a load generator's output. It also redacts
// the user information and query string of every http or https URL in text,
// because tools re-print URLs in altered forms: k6, for one, shows
// "user:***@" in place of "user:password@".
func RedactText(text, targetURL string) string {
	if u, err := url.Parse(targetURL); err == nil {
		var secrets []string
		if u.RawQuery != "" {
			secrets = append(secrets, u.RawQuery)
		}
		if u.User != nil {
			secrets = append(secrets, u.User.String())
		}
		if userinfo := rawUserInfo(targetURL); userinfo != "" {
			secrets = append(secrets, userinfo)
		}
		for _, s := range secrets {
			text = strings.ReplaceAll(text, s, redacted)
			// The forms a quoting logger (such as k6's) prints.
			if quoted := quoteEscaper.Replace(s); quoted != s {
				text = strings.ReplaceAll(text, quoted, redacted)
			}
		}
	}
	text = urlUserInfoPattern.ReplaceAllString(text, "${1}"+redacted+"@")
	return urlQueryPattern.ReplaceAllString(text, "${1}?"+redacted)
}

// rawUserInfo returns the user information exactly as written in rawURL.
func rawUserInfo(rawURL string) string {
	_, rest, ok := strings.Cut(rawURL, "://")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		return rest[:i]
	}
	return ""
}

const redacted = "xxxxx"

var (
	quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	// urlUserInfoPattern matches "http(s)://<user info>@".
	urlUserInfoPattern = regexp.MustCompile(`(?i)(\bhttps?://)[^\s/?#@"'\\<>]+@`)
	// urlQueryPattern matches "http(s)://<authority and path>?<query>".
	urlQueryPattern = regexp.MustCompile(`(?i)(\bhttps?://[^\s?#"'\\<>]*)\?[^\s#"'\\<>]*`)
)

// Result is what a completed run measured. All values come from the load
// generator's own report; none are estimated.
type Result struct {
	// Runner names the load generator, for example "k6", and its version.
	Runner        string `json:"runner"`
	RunnerVersion string `json:"runner_version,omitempty"`

	// TargetURL is redacted by RedactURL (no user information or query).
	TargetURL                 string    `json:"target_url"`
	VUs                       int       `json:"vus"`
	ConfiguredDurationSeconds float64   `json:"configured_duration_seconds"`
	StartedAt                 time.Time `json:"started_at"`
	FinishedAt                time.Time `json:"finished_at"`

	Metrics Metrics `json:"metrics"`
}

// Metrics are the client-side measurements of a run.
type Metrics struct {
	// DurationSeconds is how long the load generator measured the run.
	DurationSeconds float64 `json:"duration_seconds"`
	// HTTPRequests counts every request sent, including failed ones.
	HTTPRequests int64 `json:"http_requests"`
	// FailedRequests counts requests that failed: a transport error, or a
	// response status outside 200-399.
	FailedRequests int64 `json:"failed_requests"`
	// FailureRate is FailedRequests / HTTPRequests, from 0 to 1.
	FailureRate float64 `json:"failure_rate"`
	// RequestsPerSecond is HTTPRequests / DurationSeconds.
	RequestsPerSecond float64 `json:"requests_per_second"`
	// LatencyMs is the request duration distribution over all requests.
	LatencyMs Latency `json:"latency_ms"`
}

// Latency is a request duration distribution in milliseconds.
type Latency struct {
	Avg float64 `json:"avg"`
	Min float64 `json:"min"`
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

// Runner applies a workload to a target and reports what it measured.
//
// Run validates spec before doing anything. It returns an error, never a
// partial Result, if the run could not be completed or its results could not
// be read; a completed run whose requests failed is a Result, judged by
// Evaluate.
type Runner interface {
	Run(ctx context.Context, spec Spec) (Result, error)
}

// Verdict says whether a completed run met its pass criteria.
type Verdict struct {
	Passed         bool     `json:"passed"`
	MaxFailureRate float64  `json:"max_failure_rate"`
	Reasons        []string `json:"reasons,omitempty"`
}

// ValidateMaxFailureRate checks a pass criterion before a run starts.
func ValidateMaxFailureRate(rate float64) error {
	if math.IsNaN(rate) || rate < 0 || rate > 1 {
		return fmt.Errorf("max failure rate must be between 0 and 1, got %v", rate)
	}
	return nil
}

// Evaluate judges a completed run: it passes only if it made at least one
// request and its failure rate is at most maxFailureRate.
func Evaluate(m Metrics, maxFailureRate float64) Verdict {
	v := Verdict{Passed: true, MaxFailureRate: maxFailureRate}
	if m.HTTPRequests == 0 {
		v.Passed = false
		v.Reasons = append(v.Reasons, "no HTTP requests were made")
	}
	if m.FailureRate > maxFailureRate {
		v.Passed = false
		v.Reasons = append(v.Reasons, fmt.Sprintf("failure rate %.2f%% exceeds the allowed %.2f%%",
			m.FailureRate*100, maxFailureRate*100))
	}
	return v
}

// Report is the JSON results file of one run. Exactly one of Run and Error is
// set: a run that could not complete is reported as failed, never omitted, so
// an old successful report cannot be mistaken for the latest run.
type Report struct {
	SchemaVersion int     `json:"schema_version"`
	Run           *Result `json:"run,omitempty"`
	Error         string  `json:"error,omitempty"`
	Verdict       Verdict `json:"verdict"`
}

// ReportSchemaVersion is the current Report format.
const ReportSchemaVersion = 1
