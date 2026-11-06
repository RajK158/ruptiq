package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/RajK158/ruptiq/internal/workload"
)

// Defaults and limits for a PrometheusReader.
const (
	DefaultTimeout  = 10 * time.Second
	DefaultMaxBytes = 16 << 20 // 16 MiB, far above a typical service's few hundred KiB
	MaxEndpointLen  = 2048
)

// Errors returned by PrometheusReader. Use errors.Is to test for them; the
// error message adds the details.
var (
	// ErrInvalidEndpoint means the configured URL cannot be used.
	ErrInvalidEndpoint = errors.New("invalid metrics endpoint")
	// ErrFetch means no HTTP response arrived: the endpoint is unreachable,
	// the request timed out, or it was cancelled. The cause is also wrapped,
	// so errors.Is(err, context.DeadlineExceeded) works.
	ErrFetch = errors.New("metrics request failed")
	// ErrHTTPStatus means the endpoint answered with a status other than 200.
	ErrHTTPStatus = errors.New("metrics endpoint returned an error status")
	// ErrResponseTooLarge means the body exceeded the reader's MaxBytes.
	ErrResponseTooLarge = errors.New("metrics response too large")
	// ErrInvalidMetrics means the body is not usable Prometheus text format:
	// the wrong content type, empty, malformed, or inconsistent.
	ErrInvalidMetrics = errors.New("invalid metrics")
)

// acceptHeader asks for the Prometheus text format, version 0.0.4, which every
// Prometheus client library serves.
const acceptHeader = "text/plain;version=0.0.4;q=1,*/*;q=0.1"

// PrometheusConfig configures a PrometheusReader.
type PrometheusConfig struct {
	// URL is the metrics endpoint, for example http://localhost:8082/metrics.
	// User information in the URL is sent as HTTP basic authentication.
	URL string
	// Timeout bounds one Read, from sending the request to reading the whole
	// body. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxBytes bounds the (decompressed) response body. Zero means
	// DefaultMaxBytes.
	MaxBytes int64
	// Client sends the request. Nil means a client with Go's default
	// transport.
	Client *http.Client
}

// PrometheusReader reads a snapshot from an HTTP endpoint serving the
// Prometheus text exposition format. It works with any such endpoint and
// assumes nothing about which metrics it exposes.
type PrometheusReader struct {
	url      string
	timeout  time.Duration
	maxBytes int64
	client   *http.Client
}

var _ Reader = (*PrometheusReader)(nil)

// NewPrometheusReader validates cfg and returns a reader. It does not contact
// the endpoint.
func NewPrometheusReader(cfg PrometheusConfig) (*PrometheusReader, error) {
	if err := validateEndpoint(cfg.URL); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidEndpoint, err)
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("%w: timeout must not be negative", ErrInvalidEndpoint)
	}
	if cfg.MaxBytes < 0 {
		return nil, fmt.Errorf("%w: maximum response size must not be negative", ErrInvalidEndpoint)
	}
	r := &PrometheusReader{url: cfg.URL, timeout: cfg.Timeout, maxBytes: cfg.MaxBytes, client: cfg.Client}
	if r.timeout == 0 {
		r.timeout = DefaultTimeout
	}
	if r.maxBytes == 0 {
		r.maxBytes = DefaultMaxBytes
	}
	if r.client == nil {
		r.client = &http.Client{}
	}
	return r, nil
}

// Endpoint returns the endpoint URL redacted for display.
func (r *PrometheusReader) Endpoint() string {
	return workload.RedactURL(r.url)
}

// Read fetches the endpoint once and parses the response. Every error
// message shows the endpoint redacted.
func (r *PrometheusReader) Read(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	endpoint := r.Endpoint()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %s: %s", ErrInvalidEndpoint, endpoint, r.redact(err.Error()))
	}
	req.Header.Set("Accept", acceptHeader)

	start := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		return Snapshot{}, r.fetchError(ctx, endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("%w: GET %s: %s%s", ErrHTTPStatus, endpoint, resp.Status, r.bodyHint(resp.Body))
	}
	if err := checkContentType(resp.Header.Get("Content-Type")); err != nil {
		return Snapshot{}, fmt.Errorf("%w: GET %s: %v", ErrInvalidMetrics, endpoint, err)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, r.maxBytes+1))
	if err != nil {
		return Snapshot{}, r.fetchError(ctx, endpoint, err)
	}
	if int64(len(body)) > r.maxBytes {
		return Snapshot{}, fmt.Errorf("%w: GET %s: body exceeds %d bytes", ErrResponseTooLarge, endpoint, r.maxBytes)
	}
	elapsed := time.Since(start)

	families, err := parse(body)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: GET %s: %s", ErrInvalidMetrics, endpoint, r.redact(err.Error()))
	}
	return Snapshot{
		Endpoint:              endpoint,
		ScrapedAt:             start.UTC(),
		ScrapeDurationSeconds: elapsed.Seconds(),
		Families:              families,
	}, nil
}

// fetchError describes a request that got no complete response. Go's
// *url.Error repeats the full URL (only the password is masked), so it is
// unwrapped and the URL replaced by the redacted one.
func (r *PrometheusReader) fetchError(ctx context.Context, endpoint string, err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: GET %s: no complete response within the deadline (timeout %v): %w",
			ErrFetch, endpoint, r.timeout, context.DeadlineExceeded)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("%w: GET %s: %w", ErrFetch, endpoint, context.Canceled)
	}
	return fmt.Errorf("%w: GET %s: %w", ErrFetch, endpoint, &redactedError{msg: r.redact(err.Error()), err: err})
}

// redactedError shows a redacted message but still unwraps to the original
// error, so errors.Is and errors.As keep working.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// bodyHint returns the start of an error response body, redacted and on one
// line, so a message such as "404 page not found" is visible.
func (r *PrometheusReader) bodyHint(body io.Reader) string {
	const limit = 200
	data, _ := io.ReadAll(io.LimitReader(body, limit+1))
	text := strings.Join(strings.FieldsFunc(string(data), func(c rune) bool {
		return unicode.IsSpace(c) || unicode.IsControl(c)
	}), " ")
	if text == "" {
		return ""
	}
	if len(data) > limit {
		text += " ..."
	}
	return ": " + r.redact(text)
}

func (r *PrometheusReader) redact(text string) string {
	return workload.RedactText(text, r.url)
}

func validateEndpoint(raw string) error {
	if raw == "" {
		return errors.New("URL is required")
	}
	if len(raw) > MaxEndpointLen {
		return fmt.Errorf("URL must be at most %d characters", MaxEndpointLen)
	}
	if strings.IndexFunc(raw, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) >= 0 {
		return errors.New("URL must not contain spaces or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("URL is not valid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("URL must start with http:// or https://")
	}
	if u.Hostname() == "" {
		return errors.New("URL must include a host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return errors.New("URL port must be between 1 and 65535")
		}
	}
	if u.Fragment != "" {
		return errors.New("URL must not contain a fragment")
	}
	return nil
}

// checkContentType accepts the Prometheus text format (text/plain) and a
// missing Content-Type. OpenMetrics, protobuf, HTML, and JSON are rejected
// rather than misread.
func checkContentType(value string) error {
	if value == "" {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return fmt.Errorf("unreadable Content-Type %q", value)
	}
	if mediaType != "text/plain" {
		return fmt.Errorf("unsupported Content-Type %q: want the Prometheus text format (text/plain)", mediaType)
	}
	return nil
}

// parse turns a text exposition into families sorted by name.
func parse(body []byte) ([]Family, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("response body is empty")
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	parsed, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if len(parsed) == 0 {
		return nil, errors.New("response contains no samples")
	}

	families := make([]Family, 0, len(parsed))
	for _, mf := range parsed {
		f, err := convertFamily(mf)
		if err != nil {
			return nil, fmt.Errorf("metric %s: %w", mf.GetName(), err)
		}
		families = append(families, f)
	}
	slices.SortFunc(families, func(a, b Family) int { return strings.Compare(a.Name, b.Name) })
	return families, nil
}

func convertFamily(mf *dto.MetricFamily) (Family, error) {
	f := Family{Name: mf.GetName(), Help: mf.GetHelp()}
	switch mf.GetType() {
	case dto.MetricType_COUNTER:
		f.Type = Counter
	case dto.MetricType_GAUGE:
		f.Type = Gauge
	case dto.MetricType_HISTOGRAM:
		f.Type = Histogram
	case dto.MetricType_SUMMARY:
		f.Type = Summary
	case dto.MetricType_UNTYPED:
		f.Type = Untyped
	default:
		return Family{}, fmt.Errorf("unsupported metric type %v", mf.GetType())
	}

	for _, m := range mf.GetMetric() {
		s := Series{Labels: labels(m)}
		if ms := m.GetTimestampMs(); ms != 0 {
			t := time.UnixMilli(ms).UTC()
			s.Timestamp = &t
		}
		switch f.Type {
		case Counter:
			v := Float(m.GetCounter().GetValue())
			s.Value = &v
		case Gauge:
			v := Float(m.GetGauge().GetValue())
			s.Value = &v
		case Untyped:
			v := Float(m.GetUntyped().GetValue())
			s.Value = &v
		case Histogram:
			h, err := convertHistogram(m.GetHistogram())
			if err != nil {
				return Family{}, fmt.Errorf("series %s: %w", labelString(s.Labels), err)
			}
			s.Histogram = h
		case Summary:
			s.Summary = convertSummary(m.GetSummary())
		}
		f.Series = append(f.Series, s)
	}
	slices.SortFunc(f.Series, func(a, b Series) int {
		return strings.Compare(labelString(a.Labels), labelString(b.Labels))
	})
	return f, nil
}

// convertHistogram copies a classic histogram, sorted by upper bound, rejecting
// counts that are not whole numbers, duplicate bounds, counts that decrease,
// and a +Inf bucket that disagrees with the count.
func convertHistogram(h *dto.Histogram) (*HistogramValue, error) {
	if h.SampleCountFloat != nil {
		return nil, errors.New("histogram count is not a whole, non-negative number")
	}
	out := &HistogramValue{Count: h.GetSampleCount(), Sum: Float(h.GetSampleSum())}
	for _, b := range h.GetBucket() {
		if b.CumulativeCountFloat != nil {
			return nil, fmt.Errorf("bucket le=%v count is not a whole, non-negative number", b.GetUpperBound())
		}
		out.Buckets = append(out.Buckets, Bucket{UpperBound: Float(b.GetUpperBound()), CumulativeCount: b.GetCumulativeCount()})
	}
	slices.SortStableFunc(out.Buckets, func(a, b Bucket) int {
		return compareFloat(float64(a.UpperBound), float64(b.UpperBound))
	})
	for i, b := range out.Buckets {
		if math.IsNaN(float64(b.UpperBound)) {
			return nil, errors.New("bucket upper bound is NaN")
		}
		if i > 0 {
			prev := out.Buckets[i-1]
			if b.UpperBound == prev.UpperBound {
				return nil, fmt.Errorf("duplicate bucket le=%v", float64(b.UpperBound))
			}
			if b.CumulativeCount < prev.CumulativeCount {
				return nil, fmt.Errorf("bucket counts decrease: le=%v has %d, le=%v has %d",
					float64(prev.UpperBound), prev.CumulativeCount, float64(b.UpperBound), b.CumulativeCount)
			}
		}
		if math.IsInf(float64(b.UpperBound), 1) && b.CumulativeCount != out.Count {
			return nil, fmt.Errorf("+Inf bucket has %d observations but count is %d", b.CumulativeCount, out.Count)
		}
	}
	return out, nil
}

func convertSummary(s *dto.Summary) *SummaryValue {
	out := &SummaryValue{Count: s.GetSampleCount(), Sum: Float(s.GetSampleSum())}
	for _, q := range s.GetQuantile() {
		out.Quantiles = append(out.Quantiles, Quantile{Quantile: q.GetQuantile(), Value: Float(q.GetValue())})
	}
	slices.SortFunc(out.Quantiles, func(a, b Quantile) int { return compareFloat(a.Quantile, b.Quantile) })
	return out
}

func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func labels(m *dto.Metric) map[string]string {
	if len(m.GetLabel()) == 0 {
		return nil
	}
	l := make(map[string]string, len(m.GetLabel()))
	for _, p := range m.GetLabel() {
		l[p.GetName()] = p.GetValue()
	}
	return l
}

// labelString formats labels as {a="1",b="2"}, sorted by name, with values
// quoted as Go strings so control characters are escaped. It returns "" for
// no labels.
func labelString(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	names := make([]string, 0, len(l))
	for n := range l {
		names = append(names, n)
	}
	slices.Sort(names)
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(strconv.Quote(l[n]))
	}
	b.WriteByte('}')
	return b.String()
}
