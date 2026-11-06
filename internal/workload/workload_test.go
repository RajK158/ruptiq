package workload

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"
)

func validSpec() Spec {
	return Spec{TargetURL: "http://127.0.0.1:8082/inventory/LAPTOP-001", VUs: DefaultVUs, Duration: DefaultDuration}
}

func TestSpecValidateAccepts(t *testing.T) {
	for _, mod := range []struct {
		name string
		edit func(*Spec)
	}{
		{"defaults", func(*Spec) {}},
		{"https with query", func(s *Spec) { s.TargetURL = "https://example.com/path?q=1&r=2" }},
		{"hostname without path", func(s *Spec) { s.TargetURL = "http://localhost:8080" }},
		{"ipv6 host", func(s *Spec) { s.TargetURL = "http://[::1]:8082/healthz" }},
		{"user info", func(s *Spec) { s.TargetURL = "https://user:secret@example.com/" }},
		{"minimum load", func(s *Spec) { s.VUs, s.Duration = 1, MinDuration }},
		{"maximum load", func(s *Spec) { s.VUs, s.Duration = MaxVUs, MaxDuration }},
		{"longest URL", func(s *Spec) {
			s.TargetURL = "http://example.com/" + strings.Repeat("a", MaxTargetURLLen-len("http://example.com/"))
		}},
	} {
		t.Run(mod.name, func(t *testing.T) {
			s := validSpec()
			mod.edit(&s)
			if err := s.Validate(); err != nil {
				t.Fatalf("Validate(%+v) = %v, want nil", s, err)
			}
		})
	}
}

func TestSpecValidateRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Spec)
		want string
	}{
		{"missing URL", func(s *Spec) { s.TargetURL = "" }, "target URL is required"},
		{"relative URL", func(s *Spec) { s.TargetURL = "/inventory/LAPTOP-001" }, "http:// or https://"},
		{"no scheme", func(s *Spec) { s.TargetURL = "localhost:8082/healthz" }, "http:// or https://"},
		{"ftp scheme", func(s *Spec) { s.TargetURL = "ftp://example.com/file" }, "http:// or https://"},
		{"file scheme", func(s *Spec) { s.TargetURL = "file:///etc/passwd" }, "http:// or https://"},
		{"no host", func(s *Spec) { s.TargetURL = "http:///path" }, "must include a host"},
		{"port zero", func(s *Spec) { s.TargetURL = "http://localhost:0/" }, "port must be between"},
		{"port too large", func(s *Spec) { s.TargetURL = "http://localhost:65536/" }, "port must be between"},
		{"bad port", func(s *Spec) { s.TargetURL = "http://localhost:http/" }, "not a valid URL"},
		{"fragment", func(s *Spec) { s.TargetURL = "http://localhost/#top" }, "fragment"},
		{"space", func(s *Spec) { s.TargetURL = "http://localhost/a b" }, "spaces or control"},
		{"newline", func(s *Spec) { s.TargetURL = "http://localhost/\nX-Injected: 1" }, "spaces or control"},
		{"too long", func(s *Spec) {
			s.TargetURL = "http://example.com/" + strings.Repeat("a", MaxTargetURLLen)
		}, "at most 2048"},
		{"zero VUs", func(s *Spec) { s.VUs = 0 }, "vus must be between 1 and 1000, got 0"},
		{"negative VUs", func(s *Spec) { s.VUs = -1 }, "vus must be between"},
		{"too many VUs", func(s *Spec) { s.VUs = MaxVUs + 1 }, "vus must be between"},
		{"zero duration", func(s *Spec) { s.Duration = 0 }, "duration must be between"},
		{"too short", func(s *Spec) { s.Duration = 500 * time.Millisecond }, "duration must be between"},
		{"too long duration", func(s *Spec) { s.Duration = MaxDuration + time.Second }, "duration must be between"},
		{"fractional seconds", func(s *Spec) { s.Duration = 1500 * time.Millisecond }, "whole number of seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.edit(&s)
			err := s.Validate()
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Validate(%+v) = %v, want *ValidationError", s, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestSpecValidateReportsEveryProblem(t *testing.T) {
	err := Spec{}.Validate()
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Validate() = %v, want *ValidationError", err)
	}
	if len(verr.Problems) != 3 {
		t.Fatalf("problems = %q, want 3 (URL, VUs, duration)", verr.Problems)
	}
}

// sensitiveURLs are target URLs with secrets, the redacted form, and the
// secret strings that must never appear in output.
var sensitiveURLs = []struct {
	name, url, redacted string
	secrets             []string
}{
	{"user and password", "https://alice:pw-SECRET1@example.com/inventory/LAPTOP-001",
		"https://xxxxx@example.com/inventory/LAPTOP-001", []string{"alice", "pw-SECRET1"}},
	{"user only (token as user name)", "https://TOKEN-SECRET0@example.com/",
		"https://xxxxx@example.com/", []string{"TOKEN-SECRET0"}},
	{"api_key query", "http://127.0.0.1:8082/inventory/LAPTOP-001?api_key=KEY-SECRET2",
		"http://127.0.0.1:8082/inventory/LAPTOP-001?xxxxx", []string{"api_key", "KEY-SECRET2"}},
	{"token query", "http://localhost:8082/healthz?token=TOK-SECRET3",
		"http://localhost:8082/healthz?xxxxx", []string{"token=", "TOK-SECRET3"}},
	{"several query parameters", "https://example.com/search?q=laptop&api_key=KEY-SECRET2&page=2&token=TOK-SECRET3",
		"https://example.com/search?xxxxx", []string{"KEY-SECRET2", "TOK-SECRET3", "q=laptop", "page=2"}},
	{"everything", "https://alice:pw-SECRET1@[::1]:8443/a/b?api_key=KEY-SECRET2&token=TOK-SECRET3",
		"https://xxxxx@[::1]:8443/a/b?xxxxx", []string{"alice", "pw-SECRET1", "KEY-SECRET2", "TOK-SECRET3"}},
	{"encoded characters", "https://us%40er:p%3Ass%22SECRET4@example.com/?sig=a%2Fb%3D%3DSECRET5",
		"https://xxxxx@example.com/?xxxxx", []string{"SECRET4", "SECRET5", "us%40er", "us@er"}},
}

func TestRedactURL(t *testing.T) {
	for _, tc := range sensitiveURLs {
		t.Run(tc.name, func(t *testing.T) {
			if err := (Spec{TargetURL: tc.url, VUs: 1, Duration: MinDuration}).Validate(); err != nil {
				t.Fatalf("test URL is not a valid target: %v", err)
			}
			got := RedactURL(tc.url)
			if got != tc.redacted {
				t.Errorf("RedactURL = %q, want %q", got, tc.redacted)
			}
			for _, s := range tc.secrets {
				if strings.Contains(got, s) {
					t.Errorf("RedactURL = %q leaks %q", got, s)
				}
			}
		})
	}
	for _, tc := range []struct{ in, want string }{
		{"http://127.0.0.1:8082/inventory/LAPTOP-001", "http://127.0.0.1:8082/inventory/LAPTOP-001"},
		{"https://example.com", "https://example.com"},
		{"http://example.com/a%20b/", "http://example.com/a%20b/"},
		{"http://%zz", "(invalid URL)"},
	} {
		if got := RedactURL(tc.in); got != tc.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRedactText(t *testing.T) {
	for _, tc := range sensitiveURLs {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.url)
			if err != nil {
				t.Fatal(err)
			}
			// What Go's HTTP client, and so k6, prints.
			stripped := *u
			if _, ok := u.User.Password(); ok {
				stripped.User = url.UserPassword(u.User.Username(), "***")
			}
			text := strings.Join([]string{
				"plain: " + tc.url,
				"quoted: " + fmt.Sprintf("%q", tc.url),
				// As k6 logs a failed request (Go's HTTP client hides only the password).
				`level=warning msg="Request Failed" error="Get \"` + stripped.String() + `\": dial tcp: connection refused"`,
				"parsed: " + u.String(),
				"no scheme: " + u.Host + u.EscapedPath() + "?" + u.RawQuery,
			}, "\n")
			got := RedactText(text, tc.url)
			for _, s := range tc.secrets {
				if strings.Contains(got, s) {
					t.Errorf("RedactText leaks %q:\n%s", s, got)
				}
			}
			if !strings.Contains(got, "connection refused") || !strings.Contains(got, u.Host) {
				t.Errorf("RedactText lost the rest of the text:\n%s", got)
			}
		})
	}
}

func TestRedactTextRealK6Output(t *testing.T) {
	// Lines printed by k6 v2.3.0 for these targets.
	for _, tc := range []struct{ target, line string }{
		{"http://alice:pw-SECRET1@127.0.0.1:1/p?api_key=KEY-SECRET2&token=TOK-SECRET3&q=1",
			`time="2026-11-06T03:09:46-08:00" level=warning msg="Request Failed" error="Get \"http://alice:***@127.0.0.1:1/p?api_key=KEY-SECRET2&token=TOK-SECRET3&q=1\": dial tcp 127.0.0.1:1: connectex: No connection could be made because the target machine actively refused it."`},
		{"http://alice:pw-SECRET1@no-such-host.invalid/p?api_key=KEY-SECRET2",
			`time="2026-11-06T03:09:46-08:00" level=warning msg="Request Failed" error="Get \"http://alice:***@no-such-host.invalid/p?api_key=KEY-SECRET2\": lookup no-such-host.invalid: no such host"`},
	} {
		got := RedactText(tc.line, tc.target)
		for _, s := range []string{"alice", "SECRET"} {
			if strings.Contains(got, s) {
				t.Errorf("RedactText leaks %q: %s", s, got)
			}
		}
		if !strings.Contains(got, `Get \"http://xxxxx@`) || !strings.Contains(got, `/p?xxxxx\"`) {
			t.Errorf("RedactText = %s", got)
		}
	}
}

func TestRedactTextLeavesOrdinaryOutput(t *testing.T) {
	const text = `level=warning msg="Request Failed" error="Get \"http://127.0.0.1:1/\": dial tcp: connection refused"`
	if got := RedactText(text, "http://127.0.0.1:1/"); got != text {
		t.Fatalf("RedactText changed output without secrets:\n%s", got)
	}
}

func TestValidateMaxFailureRate(t *testing.T) {
	for _, ok := range []float64{0, 0.05, 1} {
		if err := ValidateMaxFailureRate(ok); err != nil {
			t.Errorf("ValidateMaxFailureRate(%v) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []float64{-0.01, 1.01, math.NaN(), math.Inf(1)} {
		if err := ValidateMaxFailureRate(bad); err == nil {
			t.Errorf("ValidateMaxFailureRate(%v) = nil, want error", bad)
		}
	}
}

func TestEvaluate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		m       Metrics
		max     float64
		pass    bool
		reasons []string
	}{
		{"no failures", Metrics{HTTPRequests: 100}, 0, true, nil},
		{"one failure, none allowed", Metrics{HTTPRequests: 100, FailedRequests: 1, FailureRate: 0.01}, 0,
			false, []string{"failure rate 1.00% exceeds the allowed 0.00%"}},
		{"failures within allowance", Metrics{HTTPRequests: 100, FailedRequests: 5, FailureRate: 0.05}, 0.05, true, nil},
		{"every request failed", Metrics{HTTPRequests: 10, FailedRequests: 10, FailureRate: 1}, 0.5,
			false, []string{"failure rate 100.00% exceeds the allowed 50.00%"}},
		{"no requests", Metrics{}, 1, false, []string{"no HTTP requests were made"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Evaluate(tc.m, tc.max)
			if v.Passed != tc.pass || v.MaxFailureRate != tc.max || strings.Join(v.Reasons, "|") != strings.Join(tc.reasons, "|") {
				t.Fatalf("Evaluate = %+v, want passed=%v reasons=%q", v, tc.pass, tc.reasons)
			}
		})
	}
}
