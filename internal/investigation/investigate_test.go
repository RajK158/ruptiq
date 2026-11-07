package investigation

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/experiment"
)

func TestStablePerformance(t *testing.T) {
	inv := investigate(t, build(steady(6, get(), post())...))
	if inv.Outcome != OutcomeNoMaterialChange || len(inv.Findings) != 0 {
		t.Fatalf("outcome %s, findings %+v", inv.Outcome, inv.Findings)
	}
	if len(inv.Assessments) != 6 {
		t.Fatalf("%d assessments, want 2 routes x 3 signals", len(inv.Assessments))
	}
	for _, a := range inv.Assessments {
		if a.Status != StatusNoMaterialChange || !slices.Equal(a.BaselineWindows, []int{1, 2, 3}) || !slices.Equal(a.ComparisonWindows, []int{4, 5, 6}) {
			t.Errorf("assessment %+v", a)
		}
	}
	if !slices.Equal(inv.Windows.Comparable, []int{1, 2, 3, 4, 5, 6}) || len(inv.Windows.Excluded) != 0 {
		t.Errorf("windows = %+v", inv.Windows)
	}
}

// p95 0.8 ms (bucket 0.5-1 ms) in windows 1-2, then 2 ms (bucket 1-2.5 ms):
// +150%, three times the +50% threshold.
func TestLatencyDegradation(t *testing.T) {
	slow := get().latency(0.002, 0.001, 0.0025)
	a := build(w(get()), w(get()), w(slow), w(slow))
	inv := investigate(t, a)
	if inv.Outcome != OutcomeChangesObserved || len(inv.Findings) != 1 {
		t.Fatalf("outcome %s, findings %+v", inv.Outcome, inv.Findings)
	}
	f := onlyFinding(t, inv, FindingLatencyDegradation)
	if f.Rank != 1 || f.Designation != DesignationObservation || f.ProvenRootCause || f.Severity != SeverityMajor ||
		f.Method != "GET" || f.Route != "/inventory/{sku}" {
		t.Errorf("finding %+v", f)
	}
	c := f.Changes[0]
	wantNear(t, "baseline p95", c.Baseline.Value, 0.0008)
	wantNear(t, "comparison p95", c.Comparison.Value, 0.002)
	wantNear(t, "relative", *c.Change.Relative, 1.5)
	wantNear(t, "threshold multiple", c.Change.ThresholdMultiple, 3)
	if c.Baseline.Unit != UnitSeconds || !c.Baseline.Estimate || *c.Baseline.BucketUpperSeconds != 0.001 || *c.Comparison.BucketLowerSeconds != 0.001 {
		t.Errorf("measures %+v %+v", c.Baseline, c.Comparison)
	}
	if !slices.Equal(f.BaselineWindows, []int{1, 2}) || !slices.Equal(f.ComparisonWindows, []int{3, 4}) {
		t.Errorf("windows %v vs %v", f.BaselineWindows, f.ComparisonWindows)
	}
	if !strings.Contains(f.Symptom, "from ~0.800 ms to ~2.000 ms (+150.0%)") {
		t.Errorf("symptom %q", f.Symptom)
	}

	// Every evidence reference resolves to the p95 it describes.
	doc := decodeJSON(t, file(t, a))
	want := map[string]float64{
		"/analysis/windows/0/routes/0/latency/p95": 0.0008, "/analysis/windows/1/routes/0/latency/p95": 0.0008,
		"/analysis/windows/2/routes/0/latency/p95": 0.002, "/analysis/windows/3/routes/0/latency/p95": 0.002,
	}
	if len(f.Evidence) != 4 {
		t.Fatalf("evidence %+v", f.Evidence)
	}
	for i, e := range f.Evidence {
		p := resolve(t, doc, e.Pointer).(map[string]any)
		if p["seconds"].(float64) != want[e.Pointer] {
			t.Errorf("%s = %v", e.Pointer, p["seconds"])
		}
		role := "baseline"
		if i >= 2 {
			role = "comparison"
		}
		if e.Role != role || e.Window != i+1 || e.Metric != analysis.DefaultDurationHistogram || len(e.Series) != 1 {
			t.Errorf("evidence %+v", e)
		}
	}
}

// Every estimate lies in the 0.5-1 ms bucket: +58% is within the bucket's
// resolution, so it is not reported.
func TestLatencyWithinOneBucketIsInconclusive(t *testing.T) {
	inv := investigate(t, build(w(get().latency(0.0006, 0.0005, 0.001)), w(get().latency(0.0006, 0.0005, 0.001)),
		w(get().latency(0.00095, 0.0005, 0.001)), w(get().latency(0.00095, 0.0005, 0.001))))
	if len(inv.Findings) != 0 || inv.Outcome != OutcomeNoMaterialChange {
		t.Fatalf("outcome %s, findings %+v", inv.Outcome, inv.Findings)
	}
	a := assessment(t, inv, "GET", SignalP95Latency)
	if a.Status != StatusInconclusive || !strings.Contains(a.Reason, "overlap") {
		t.Errorf("assessment %+v", a)
	}
	wantNear(t, "relative", *a.Change.Relative, 0.00095/0.0006-1)
}

// 1000 then 600 requests per one-second window: -40%, 1.33 times the -30%
// threshold.
func TestThroughputDegradation(t *testing.T) {
	low := get().requests(600, 0, 0)
	a := build(w(get()), w(get()), w(low), w(low))
	inv := investigate(t, a)
	f := onlyFinding(t, inv, FindingThroughputDegradation)
	c := f.Changes[0]
	wantNear(t, "baseline", c.Baseline.Value, 1000)
	wantNear(t, "comparison", c.Comparison.Value, 600)
	wantNear(t, "relative", *c.Change.Relative, -0.4)
	wantNear(t, "multiple", c.Change.ThresholdMultiple, 0.4/0.3)
	if f.Severity != SeverityModerate || c.Baseline.Unit != UnitRequestsPerSecond || c.Baseline.Requests != 2000 || c.Comparison.Requests != 1200 {
		t.Errorf("finding %+v", f)
	}
	if len(f.Contradictions) != 1 || !strings.Contains(f.Contradictions[0], "p95 latency did not rise materially") {
		t.Errorf("contradictions %q", f.Contradictions)
	}
	doc := decodeJSON(t, file(t, a))
	r := resolve(t, doc, f.Evidence[3].Pointer).(map[string]any)
	if f.Evidence[3].Pointer != "/analysis/windows/3/routes/0/requests" || r["per_second"].(float64) != 600 {
		t.Errorf("evidence %s = %v", f.Evidence[3].Pointer, r)
	}
}

func TestCombinedDegradation(t *testing.T) {
	bad := get().requests(600, 0, 0).latency(0.002, 0.001, 0.0025)
	inv := investigate(t, build(w(get()), w(get()), w(bad), w(bad)))
	var types []string
	for _, f := range inv.Findings {
		types = append(types, f.Type)
	}
	// Latency is major (3x); the combined finding takes the smaller multiple
	// (throughput's 1.33x), so it is moderate and ranks after it.
	if !slices.Equal(types, []string{FindingLatencyDegradation, FindingCombinedDegradation, FindingThroughputDegradation}) {
		t.Fatalf("findings %v", types)
	}
	c := inv.Findings[1]
	if c.Designation != DesignationHypothesis || c.ProvenRootCause || len(c.Changes) != 2 || len(c.Evidence) != 8 ||
		!slices.Equal(c.Related, []string{inv.Findings[0].ID, inv.Findings[2].ID}) {
		t.Errorf("combined %+v", c)
	}
	wantNear(t, "combined multiple", c.Ranking.ThresholdMultiple, 0.4/0.3)
	if !strings.Contains(strings.Join(c.Limitations, " "), "does not show that one causes the other") {
		t.Errorf("limitations %q", c.Limitations)
	}
}

func TestLatencyAndThroughputOverDifferentWindowsAreNotCombined(t *testing.T) {
	bad := get().requests(600, 0, 0).latency(0.002, 0.001, 0.0025)
	// Window 3's latency is unavailable: throughput compares 1-3 with 4-6,
	// latency 1-2 with 5-6 (window 4 is its middle window).
	noLat := get().withoutLatency("not comparable: the bucket bounds changed")
	inv := investigate(t, build(w(get()), w(get()), w(noLat), w(bad), w(bad), w(bad)))
	if len(findingsOf(inv, FindingThroughputDegradation)) != 1 {
		t.Fatalf("findings %+v", inv.Findings)
	}
	if len(findingsOf(inv, FindingCombinedDegradation)) != 0 {
		t.Fatalf("combined finding over different windows: %+v", inv.Findings)
	}
	lat := onlyFinding(t, inv, FindingLatencyDegradation)
	if !strings.Contains(strings.Join(lat.Limitations, " "), "over different windows") {
		t.Errorf("limitations %q", lat.Limitations)
	}
}

// 0 of 2000 requests were 5xx, then 120 of 2000: from 0% to 6%, six times
// the one-point threshold. The baseline is zero, so there is no relative
// change.
func TestServerErrorIncreaseFromZero(t *testing.T) {
	bad := get().requests(940, 60, 0)
	clientErrors := post().requests(500, 0, 500)
	inv := investigate(t, build(w(get(), post()), w(get(), post()), w(bad, clientErrors), w(bad, clientErrors)))
	if len(inv.Findings) != 1 {
		t.Fatalf("findings %+v", inv.Findings)
	}
	f := onlyFinding(t, inv, FindingServerErrorIncrease)
	c := f.Changes[0]
	if c.Baseline.Value != 0 || *c.Baseline.ServerErrors != 0 || *c.Comparison.ServerErrors != 120 || c.Change.Relative != nil ||
		c.Change.RelativeUnavailable != "the baseline is zero" || f.Severity != SeverityMajor || c.Baseline.Unit != UnitRatio {
		t.Errorf("finding %+v", f)
	}
	wantNear(t, "comparison ratio", c.Comparison.Value, 0.06)
	wantNear(t, "multiple", c.Change.ThresholdMultiple, 6)
	if !strings.Contains(f.Symptom, "from 0.00% (0 of 2000 requests) to 6.00% (120 of 2000 requests)") {
		t.Errorf("symptom %q", f.Symptom)
	}
	// An error spike without a latency increase says so.
	if !strings.Contains(strings.Join(f.Contradictions, " "), "not accompanied by a slowdown") {
		t.Errorf("contradictions %q", f.Contradictions)
	}
	if !strings.Contains(strings.Join(f.Limitations, " "), "does not by itself show lower service capacity") {
		t.Errorf("limitations %q", f.Limitations)
	}
	// 4xx responses are not server errors.
	if a := assessment(t, inv, "POST", SignalServerErrorRatio); a.Status != StatusNoMaterialChange || a.Comparison.Value != 0 {
		t.Errorf("POST %+v", a)
	}
}

func TestServerErrorIncreaseThatIsNotRobust(t *testing.T) {
	for _, tc := range []struct {
		name       string
		base, comp spec
		want       string
	}{
		// 2% to 3.5%: +1.5 points, but only 1.75 times the baseline.
		{"factor", get().requests(980, 20, 0), get().requests(965, 35, 0), "1.75 times the baseline's"},
		// 0% to 2% of 200 requests: only 4 errors.
		{"few errors", get().requests(100, 0, 0), get().requests(98, 2, 0), "only 4 5xx responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := investigate(t, build(w(tc.base), w(tc.base), w(tc.comp), w(tc.comp)))
			a := assessment(t, inv, "GET", SignalServerErrorRatio)
			if len(findingsOf(inv, FindingServerErrorIncrease)) != 0 || a.Status != StatusInconclusive || !strings.Contains(a.Reason, tc.want) {
				t.Errorf("assessment %+v, findings %+v", a, inv.Findings)
			}
		})
	}
}

// Medians 700 and 400 requests/s fall 43%, but window 3 (500) is above
// window 2 (400): noise cannot be ruled out.
func TestNoisyThroughputIsInconclusive(t *testing.T) {
	inv := investigate(t, build(w(get()), w(get().requests(400, 0, 0)), w(get().requests(500, 0, 0)), w(get().requests(300, 0, 0))))
	a := assessment(t, inv, "GET", SignalThroughput)
	if a.Status != StatusInconclusive || len(findingsOf(inv, FindingThroughputDegradation)) != 0 {
		t.Errorf("assessment %+v", a)
	}
	wantNear(t, "baseline median", a.Baseline.Value, 700)
	wantNear(t, "comparison median", a.Comparison.Value, 400)
}

// Unequal windows: rates are per measured second, so 2000 requests in 2s
// equal 1000 in 1s.
func TestUnequalWindowLengths(t *testing.T) {
	inv := investigate(t, build(w(get()), w(get().requests(2000, 0, 0)).secs(2), w(get().requests(500, 0, 0)).secs(0.5), w(get())))
	a := assessment(t, inv, "GET", SignalThroughput)
	if a.Status != StatusNoMaterialChange {
		t.Fatalf("assessment %+v", a)
	}
	wantNear(t, "baseline", a.Baseline.Value, 1000)
	wantNear(t, "comparison", a.Comparison.Value, 1000)
	wantNear(t, "baseline elapsed", a.Baseline.ElapsedSeconds, 3)
}

func TestZeroBaseline(t *testing.T) {
	idle := get().requests(0, 0, 0).observations(0).withoutP95("no observations")
	inv := investigate(t, build(w(idle), w(idle), w(get()), w(get())))
	if inv.Outcome != OutcomeInsufficientEvidence {
		t.Fatalf("outcome %s", inv.Outcome)
	}
	f := onlyFinding(t, inv, FindingInsufficientEvidence)
	if len(f.Gaps) != 3 || f.Designation != DesignationAbstention || f.Severity != "" {
		t.Fatalf("finding %+v", f)
	}
	if !strings.Contains(f.Gaps[0].Reason, "the baseline has 0 requests, fewer than 100") {
		t.Errorf("throughput gap %+v", f.Gaps[0])
	}
	// Windows without requests have no 5xx ratio or p95: they are gaps, not
	// zeros.
	if len(f.Gaps[1].Gaps) != 2 || f.Gaps[1].Gaps[0].Reason != "no requests in this window" || len(f.Gaps[2].Gaps) != 2 {
		t.Errorf("gaps %+v", f.Gaps)
	}
	if a := assessment(t, inv, "GET", SignalThroughput); a.Change != nil || a.Baseline != nil {
		t.Errorf("a change was calculated from a zero baseline: %+v", a)
	}
}

func TestMultipleRoutesAreRanked(t *testing.T) {
	put := rt("PUT", "/items")
	del := rt("DELETE", "/items")
	patch := rt("PATCH", "/a")
	base := []spec{get(), post().requests(990, 10, 0), put, patch}
	comp := []spec{
		get().latency(0.002, 0.001, 0.0025), // +150%: 3x, major
		post().requests(975, 25, 0),         // 1% to 2.5%: 1.5x, moderate
		put,                                 // stable
		patch.latency(0.004, 0.0025, 0.005), // +400%: 8x, major
		del,                                 // only in the later windows
	}
	inv := investigate(t, build(w(base...), w(base...), w(comp...), w(comp...)))
	var got []string
	for _, f := range inv.Findings {
		got = append(got, f.Type+" "+f.Method)
	}
	want := []string{
		"latency_degradation PATCH",
		"latency_degradation GET",
		"server_error_increase POST",
		"insufficient_evidence DELETE",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ranking:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i, f := range inv.Findings {
		if f.Rank != i+1 || f.Ranking.Rule != RankingRule {
			t.Errorf("finding %d rank %d", i, f.Rank)
		}
	}
	del0 := inv.Findings[3].Gaps[0]
	if len(del0.Gaps) != 2 || del0.Gaps[0].Reason != "the route has no series in this window" {
		t.Errorf("DELETE gaps %+v", del0)
	}
	wantNear(t, "POST multiple", inv.Findings[2].Ranking.ThresholdMultiple, 1.5)
}

func TestDeterministicOrdering(t *testing.T) {
	put := rt("PUT", "/items")
	base := []spec{get(), post().requests(990, 10, 0), put}
	comp := []spec{get().latency(0.002, 0.001, 0.0025), post().requests(975, 25, 0), put.requests(500, 0, 0)}
	first, _ := json.Marshal(investigate(t, build(w(base...), w(base...), w(comp...), w(comp...))))
	rng := rand.New(rand.NewPCG(1, 2))
	for range 20 {
		b, c := slices.Clone(base), slices.Clone(comp)
		rng.Shuffle(len(b), func(i, j int) { b[i], b[j] = b[j], b[i] })
		rng.Shuffle(len(c), func(i, j int) { c[i], c[j] = c[j], c[i] })
		inv := investigate(t, build(w(b...), w(b...), w(c...), w(c...)))
		// Evidence pointers name array positions, which follow the
		// analysis' route order; everything else must be identical.
		for i := range inv.Findings {
			inv.Findings[i].Evidence = nil
		}
		again, _ := json.Marshal(inv)
		var want Investigation
		_ = json.Unmarshal(first, &want)
		for i := range want.Findings {
			want.Findings[i].Evidence = nil
		}
		wantJSON, _ := json.Marshal(want)
		if !bytes.Equal(again, wantJSON) {
			t.Fatal("the investigation depends on the order of routes")
		}
	}
}

func TestStableIDs(t *testing.T) {
	slow := get().latency(0.002, 0.001, 0.0025)
	a := investigate(t, build(w(get()), w(get()), w(slow), w(slow)))
	b := investigate(t, build(w(get(), post()), w(get(), post()), w(post(), slow), w(post(), slow)))
	if a.Findings[0].ID != b.Findings[0].ID || a.Findings[0].ID != "latency_degradation-a76631a81bc2" {
		t.Errorf("IDs %s and %s", a.Findings[0].ID, b.Findings[0].ID)
	}
	if findingID(FindingLatencyDegradation, routeKey{"GET", "/a"}) == findingID(FindingLatencyDegradation, routeKey{"GET", "/b"}) ||
		findingID(FindingLatencyDegradation, routeKey{"GET", "/a"}) == findingID(FindingThroughputDegradation, routeKey{"GET", "/a"}) ||
		findingID(FindingLatencyDegradation, routeKey{"GE", "T/a"}) == findingID(FindingLatencyDegradation, routeKey{"GET", "/a"}) {
		t.Error("IDs collide")
	}
}

func TestEdgeAndGapWindowsAreExcluded(t *testing.T) {
	a := build(
		w(get()).phases(experiment.BeforeWorkload, experiment.DuringWorkload),
		w(get()),
		w(get()).missing(1).secs(2),
		w(get()), w(get()), w(get()),
		w(get()).phases(experiment.DuringWorkload, experiment.AfterWorkload),
	)
	a.Experiment.Status, a.Experiment.Completeness = experiment.StatusIncomplete, experiment.Partial
	inv := investigate(t, a)
	if !slices.Equal(inv.Windows.Comparable, []int{2, 4, 5, 6}) {
		t.Fatalf("comparable %v", inv.Windows.Comparable)
	}
	var excluded []int
	for _, e := range inv.Windows.Excluded {
		excluded = append(excluded, e.Window)
	}
	if !slices.Equal(excluded, []int{1, 3, 7}) || !strings.Contains(inv.Windows.Excluded[1].Reason, "1 missed scrape(s)") ||
		!strings.Contains(inv.Windows.Excluded[0].Reason, "start or end") || inv.Windows.Excluded[1].Pointer != "/analysis/windows/2" {
		t.Errorf("excluded %+v", inv.Windows.Excluded)
	}
	ass := assessment(t, inv, "GET", SignalThroughput)
	if ass.Status != StatusNoMaterialChange || !slices.Equal(ass.BaselineWindows, []int{2, 4}) || !slices.Equal(ass.ComparisonWindows, []int{5, 6}) {
		t.Errorf("assessment %+v", ass)
	}
	lim := strings.Join(inv.Limitations, " ")
	if !strings.Contains(lim, "status is incomplete") || !strings.Contains(lim, "Telemetry is partial") {
		t.Errorf("limitations %q", inv.Limitations)
	}
}

func TestCounterResetLeavesInsufficientEvidence(t *testing.T) {
	reset := get().withoutRequests("not comparable between samples #3 and #4: a counter decreased (possible reset)")
	a := build(w(get()), w(get()), w(reset), w(get()))
	a.Warnings = append(a.Warnings, analysis.Warning{Kind: analysis.WarnCounterReset, Window: 3, Metric: analysis.DefaultRequestsCounter,
		Labels: labels("GET", "/inventory/{sku}", "200"), Message: "counter decreased"})
	inv := investigate(t, a)
	if inv.Outcome != OutcomeNoMaterialChange {
		t.Errorf("outcome %s (latency was still compared)", inv.Outcome)
	}
	f := onlyFinding(t, inv, FindingInsufficientEvidence)
	if len(f.Gaps) != 2 || f.Gaps[0].Signal != SignalThroughput || f.Gaps[1].Signal != SignalServerErrorRatio ||
		f.Gaps[0].Gaps[0].Window != 3 || !strings.Contains(f.Gaps[0].Gaps[0].Reason, "counter decreased") {
		t.Errorf("gaps %+v", f.Gaps)
	}
	if len(f.Evidence) != 1 || f.Evidence[0].Pointer != "/analysis/warnings/0" || f.Evidence[0].Role != "warning" || f.Evidence[0].Window != 3 {
		t.Errorf("evidence %+v", f.Evidence)
	}
	if !strings.Contains(f.NextExperiment, "keep the target running") {
		t.Errorf("next %q", f.NextExperiment)
	}
}

func TestUnavailableLatency(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    spec
		want string
	}{
		{"incompatible histogram", get().withoutLatency("not comparable between samples #2 and #3: the bucket bounds changed"), "bucket bounds changed"},
		{"too few observations", get().observations(10).withoutP95("fewer than 20 observations (10)"), "p95 unavailable: fewer than 20 observations"},
		{"rank in +Inf", get().withoutP95("the p95 rank lies in the +Inf bucket, above the largest finite bound (10s)"), "+Inf bucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := investigate(t, build(w(get()), w(tc.s), w(get()), w(get())))
			a := assessment(t, inv, "GET", SignalP95Latency)
			if a.Status != StatusInsufficientEvidence || len(a.Gaps) != 1 || a.Gaps[0].Window != 2 || !strings.Contains(a.Gaps[0].Reason, tc.want) {
				t.Errorf("assessment %+v", a)
			}
			if b := assessment(t, inv, "GET", SignalThroughput); b.Status != StatusNoMaterialChange {
				t.Errorf("throughput %+v", b)
			}
		})
	}
}

// 30 observations per window give p95 estimates, but only 60 per side.
func TestLowObservationCounts(t *testing.T) {
	few := get().requests(30, 0, 0).observations(30)
	inv := investigate(t, build(steady(4, few)...))
	for _, signal := range []string{SignalThroughput, SignalServerErrorRatio, SignalP95Latency} {
		a := assessment(t, inv, "GET", signal)
		if a.Status != StatusInsufficientEvidence || !strings.Contains(a.Reason, "60") {
			t.Errorf("%s: %+v", signal, a)
		}
	}
	if inv.Outcome != OutcomeInsufficientEvidence {
		t.Errorf("outcome %s", inv.Outcome)
	}
}

func TestOddWindowCountLeavesMiddleOut(t *testing.T) {
	inv := investigate(t, build(steady(5, get())...))
	a := assessment(t, inv, "GET", SignalThroughput)
	if !slices.Equal(a.BaselineWindows, []int{1, 2}) || !slices.Equal(a.ComparisonWindows, []int{4, 5}) ||
		!strings.Contains(a.Reason, "window 3, in the middle of 5 usable windows, is in neither side") {
		t.Errorf("assessment %+v", a)
	}
}

func TestTooFewWindows(t *testing.T) {
	inv := investigate(t, build(steady(3, get())...))
	if inv.Outcome != OutcomeInsufficientEvidence || !strings.Contains(inv.Summary, "abstains") {
		t.Fatalf("outcome %s: %s", inv.Outcome, inv.Summary)
	}
	f := onlyFinding(t, inv, FindingInsufficientEvidence)
	if !strings.Contains(f.Gaps[0].Reason, "3 usable window(s) of 3 comparable; at least 4 (2 per side) are needed") ||
		!strings.Contains(f.NextExperiment, "for at least 10s") {
		t.Errorf("finding %+v", f)
	}
	// One window per side is allowed, with a warning.
	cfg := DefaultConfig()
	cfg.MinWindows = 1
	inv = investigateWith(t, build(steady(3, get())...), cfg)
	if inv.Outcome != OutcomeNoMaterialChange || !strings.Contains(strings.Join(inv.Limitations, " "), "single noisy window") {
		t.Errorf("outcome %s, limitations %q", inv.Outcome, inv.Limitations)
	}
}

func TestNoRoutesOrWindows(t *testing.T) {
	for name, a := range map[string]analysis.Analysis{"no windows": build(), "no routes": build(steady(4)...)} {
		inv := investigate(t, a)
		if inv.Outcome != OutcomeInsufficientEvidence || len(inv.Findings) != 0 || len(inv.Assessments) != 0 ||
			!strings.Contains(inv.Summary, "no route signals") {
			t.Errorf("%s: %+v", name, inv)
		}
	}
}

func TestContradictoryObservations(t *testing.T) {
	// p95 rises while the request rate rises by 50% and the mean falls.
	busy := get().requests(1500, 0, 0).latency(0.002, 0.001, 0.0025).meanSeconds(0.0003)
	inv := investigate(t, build(w(get()), w(get()), w(busy), w(busy)))
	f := onlyFinding(t, inv, FindingLatencyDegradation)
	all := strings.Join(f.Contradictions, " | ")
	if len(f.Contradictions) != 2 || !strings.Contains(all, "the request rate rose by +50.0%") || !strings.Contains(all, "the mean latency fell from 0.400 ms to 0.300 ms") {
		t.Errorf("contradictions %q", f.Contradictions)
	}

	// One comparison window has no more 5xx than the baseline.
	inv = investigate(t, build(w(get().requests(990, 10, 0)), w(get().requests(990, 10, 0)), w(get().requests(990, 10, 0)), w(get().requests(900, 100, 0))))
	e := onlyFinding(t, inv, FindingServerErrorIncrease)
	if !strings.Contains(strings.Join(e.Contradictions, " "), "in comparison window 3 the 5xx ratio (1.00%) was no higher") {
		t.Errorf("contradictions %q", e.Contradictions)
	}
}

// allFindings is an analysis with every finding type.
func allFindings() analysis.Analysis {
	bad := get().requests(600, 0, 0).latency(0.002, 0.001, 0.0025)
	errs := post().requests(900, 100, 0)
	del := rt("DELETE", "/items")
	return build(w(get(), post()), w(get(), post()), w(bad, errs, del), w(bad, errs, del))
}

func TestNeverClaimsCauses(t *testing.T) {
	inv := investigate(t, allFindings())
	seen := map[string]bool{}
	for _, f := range inv.Findings {
		seen[f.Type] = true
		if f.ProvenRootCause || f.Designation == "" || f.NextExperiment == "" || len(f.Limitations) == 0 || f.Explanation == "" || f.Symptom == "" {
			t.Errorf("finding %s is incomplete: %+v", f.ID, f)
		}
		if f.Type != FindingInsufficientEvidence && (len(f.Evidence) == 0 || len(f.Changes) == 0 || len(f.PossibleExplanations) == 0) {
			t.Errorf("finding %s lacks evidence: %+v", f.ID, f)
		}
		for _, h := range f.PossibleExplanations {
			if !strings.HasPrefix(h, "Hypothesis (untested): ") {
				t.Errorf("possible explanation is not marked as a hypothesis: %q", h)
			}
		}
	}
	for _, typ := range []string{FindingServerErrorIncrease, FindingLatencyDegradation, FindingThroughputDegradation, FindingCombinedDegradation, FindingInsufficientEvidence} {
		if !seen[typ] {
			t.Errorf("no %s finding", typ)
		}
	}
	data, _ := json.Marshal(inv)
	var summary bytes.Buffer
	if err := WriteSummary(&summary, inv); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{string(data), summary.String()} {
		lower := strings.ToLower(text)
		for _, claim := range []string{
			"root cause is", "caused by", "statistically significant", "is significant", "proves", "proven to",
			"cpu saturation", "cpu is saturated", "database bottleneck", "the database is", "network problem", "the network is",
			"confidence", "regression was", "capacity dropped", "capacity is lower",
		} {
			if strings.Contains(lower, claim) {
				t.Errorf("output claims %q", claim)
			}
		}
	}
}

func TestCredentialRedaction(t *testing.T) {
	leaky := rt("GET", "http://user:hunter2@host/p?api_key=topsecret")
	a := build(w(leaky), w(leaky), w(leaky.latency(0.002, 0.001, 0.0025)), w(leaky.latency(0.002, 0.001, 0.0025)))
	a.Experiment.TargetURL = "https://loaduser:hunter2@127.0.0.1/inventory?api_key=topsecret"
	a.Experiment.MetricsURL = "http://scraper:hunter2@127.0.0.1/metrics?token=topsecret"
	inv := investigate(t, a)
	data, _ := json.Marshal(inv)
	var summary bytes.Buffer
	_ = WriteSummary(&summary, inv)
	for _, text := range []string{string(data), summary.String()} {
		for _, s := range []string{"hunter2", "topsecret"} {
			if strings.Contains(text, s) {
				t.Errorf("output contains %q", s)
			}
		}
	}
	if inv.Source.TargetURL != "https://xxxxx@127.0.0.1/inventory?xxxxx" || len(findingsOf(inv, FindingLatencyDegradation)) != 1 {
		t.Errorf("source %+v, findings %+v", inv.Source, inv.Findings)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"windows", func(c *Config) { c.MinWindows = 0 }, "min windows per side"},
		{"requests", func(c *Config) { c.MinRequests = 0.5 }, "min requests per side"},
		{"errors", func(c *Config) { c.MinServerErrors = -1 }, "min server errors"},
		{"ratio", func(c *Config) { c.ErrorRatioIncrease = 0 }, "error ratio increase"},
		{"factor", func(c *Config) { c.ErrorRatioFactor = 0.5 }, "error ratio factor"},
		{"latency", func(c *Config) { c.LatencyIncrease = -0.1 }, "latency increase"},
		{"throughput", func(c *Config) { c.ThroughputDecrease = 1 }, "throughput decrease"},
		{"NaN", func(c *Config) { c.LatencyIncrease = nan() }, "latency increase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.edit(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if _, err := Investigate(build(), c); err == nil {
				t.Error("Investigate accepted an invalid config")
			}
		})
	}
}

func nan() float64 {
	var zero float64
	return zero / zero
}

func TestWindowList(t *testing.T) {
	for _, tc := range []struct {
		in   []int
		want string
	}{
		{nil, "none"}, {[]int{3}, "3"}, {[]int{1, 2, 3, 5, 7, 8}, "1-3, 5, 7-8"},
	} {
		if got := windowList(tc.in); got != tc.want {
			t.Errorf("windowList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
