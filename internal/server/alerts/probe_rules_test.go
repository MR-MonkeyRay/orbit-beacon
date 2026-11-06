package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/ingest"
)

const probeTarget = "https://example.test/health"

// The same finite-value rows exercise application validation and the real schema.
func probeRuleRows() []Rule {
	var rows []Rule
	for _, metric := range []string{"probe_failed", "probe_latency_ms", "probe_tls_expiry_days", "cpu_percent", "node_expiry_days", "offline"} {
		for _, comparator := range []*string{nil, ptr(">"), ptr("<"), ptr("="), ptr("")} {
			for _, threshold := range []*float64{nil, ptr(-1.0), ptr(0.0), ptr(.5), ptr(5.0), ptr(100.0), ptr(3650.0), ptr(3650.1), ptr(60000.0), ptr(60000.1)} {
				for _, target := range []*string{nil, ptr(probeTarget)} {
					rows = append(rows, Rule{Name: "Probe", Enabled: true, ScopeKind: "all", Metric: metric, Target: target, Comparator: comparator, Threshold: threshold, ForSeconds: 180, Severity: "warning"})
				}
			}
		}
	}
	return rows
}

func probeRowValid(r Rule) bool {
	if targetMetric(r.Metric) != (r.Target != nil) {
		return false
	}
	if r.Metric == "offline" {
		return r.Comparator == nil && r.Threshold == nil
	}
	if r.Comparator == nil || r.Threshold == nil {
		return false
	}
	c, v := *r.Comparator, *r.Threshold
	switch r.Metric {
	case "probe_failed":
		return c == ">" && v == 0
	case "probe_latency_ms":
		return (c == ">" || c == "<") && v > 0 && v <= 60000
	case "probe_tls_expiry_days":
		return c == "<" && v > 0 && v <= 3650
	case "node_expiry_days":
		return c == "<" && v > 0 && v <= 365
	case "probes_down":
		return (c == ">" || c == "<") && v >= 0 && v <= 5
	default:
		return (c == ">" || c == "<") && v >= 0 && v <= 100
	}
}

func TestProbeRuleValidationTable(t *testing.T) {
	for i, r := range probeRuleRows() {
		if err := ValidateRuleSyntax(r); (err == nil) != probeRowValid(r) {
			t.Fatalf("row %d: %+v: %v", i, r, err)
		}
	}
	for _, metric := range []string{"probe_failed", "probe_latency_ms", "probe_tls_expiry_days"} {
		r := Rule{Name: "Probe", ScopeKind: "all", Metric: metric, Comparator: ptr(">"), Threshold: ptr(1.0), Severity: "warning"}
		if metric == "probe_failed" {
			r.Threshold = ptr(0.0)
		}
		if metric == "probe_tls_expiry_days" {
			r.Comparator = ptr("<")
		}
		for _, target := range []string{"", "example.test", "https://has space.test", "tcp://example.test:0", "https://user@example.test", "https://example.test/#fragment"} {
			r.Target = ptr(target)
			if err := ValidateRuleSyntax(r); err == nil {
				t.Fatalf("accepted target %q", target)
			}
		}
		for _, target := range []string{probeTarget, "tcp://example.test:443", "dns://example.test", "icmp://example.test"} {
			r.Target = ptr(target)
			if err := ValidateRuleSyntax(r); err != nil {
				t.Fatalf("target %q: %v", target, err)
			}
		}
		for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			r.Threshold = ptr(value)
			if ValidateRuleSyntax(r) == nil {
				t.Fatalf("accepted %v", value)
			}
		}
	}
}

func TestProbeRuleSchemaTable(t *testing.T) {
	f := newFixture(t, 0)
	for i, r := range probeRuleRows() {
		_, err := f.db.Exec("UPDATE alert_rules SET metric=?,target=?,comparator=?,threshold=?,for_seconds=? WHERE id=?", r.Metric, r.Target, r.Comparator, r.Threshold, r.ForSeconds, f.rule.ID)
		if (err == nil) != probeRowValid(r) {
			t.Fatalf("row %d: %+v: %v", i, r, err)
		}
	}
}

func probeLatest(now time.Time, result ...v1.ProbeResult) ingest.LatestReport {
	runtime := v1.DefaultRuntimeSettings()
	runtime.ProbeTargets = []string{probeTarget}
	return ingest.LatestReport{ReceivedAt: now, Report: v1.Report{Runtime: runtime, ProbeResults: result}}
}

func TestProbeMetricValues(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, metric string
		result       []v1.ProbeResult
		want         *float64
	}{
		{"failure", "probe_failed", []v1.ProbeResult{{Target: probeTarget, OK: false}}, ptr(1.0)},
		{"success", "probe_failed", []v1.ProbeResult{{Target: probeTarget, OK: true}}, ptr(0.0)},
		{"latency above 100", "probe_latency_ms", []v1.ProbeResult{{Target: probeTarget, OK: true, LatencyMS: ptr(250.5)}}, ptr(250.5)},
		{"failed latency", "probe_latency_ms", []v1.ProbeResult{{Target: probeTarget, OK: false, LatencyMS: ptr(250.0)}}, nil},
		{"nil latency", "probe_latency_ms", []v1.ProbeResult{{Target: probeTarget, OK: true}}, nil},
		{"tls above 100 fractional", "probe_tls_expiry_days", []v1.ProbeResult{{Target: probeTarget, TLSExpiresAt: ptr(now.Add(2409 * time.Hour))}}, ptr(100.375)},
		{"tls now", "probe_tls_expiry_days", []v1.ProbeResult{{Target: probeTarget, TLSExpiresAt: &now}}, ptr(0.0)},
		{"tls past", "probe_tls_expiry_days", []v1.ProbeResult{{Target: probeTarget, TLSExpiresAt: ptr(now.Add(-time.Hour))}}, ptr(0.0)},
		{"nil tls", "probe_tls_expiry_days", []v1.ProbeResult{{Target: probeTarget}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Rule{Metric: tc.metric, Target: ptr(probeTarget)}
			value := metricValue(probeLatest(now, tc.result...), r, now)
			if (value == nil) != (tc.want == nil) || value != nil && *value != *tc.want {
				t.Fatalf("value=%v want=%v", value, tc.want)
			}
			latest := probeLatest(now, tc.result...)
			latest.ProbeResults = []v1.ProbeResult{{Target: probeTarget + "/other", OK: false}}
			if metricValue(latest, r, now) != nil {
				t.Fatal("target match was not exact")
			}
			latest = probeLatest(now, tc.result...)
			latest.ReceivedAt = now.Add(-time.Hour)
			if metricValue(latest, r, now) != nil {
				t.Fatal("stale result used")
			}
		})
	}
}

func TestProbeMetricHysteresis(t *testing.T) {
	for _, tc := range []struct {
		metric, comparator           string
		threshold, firing, recovered float64
	}{
		{"probe_failed", ">", 0, 1, 0},
		{"probe_latency_ms", ">", 10, 9.81, 9.8},
		{"probe_latency_ms", "<", 10, 10.19, 10.2},
		{"probe_latency_ms", "<", 600, 611.9, 612},
		{"probe_tls_expiry_days", "<", 10, 10.99, 11},
		{"probe_tls_expiry_days", "<", 200, 203.99, 204},
	} {
		t.Run(tc.metric+tc.comparator+fmt.Sprint(tc.threshold), func(t *testing.T) {
			r := Rule{Metric: tc.metric, Comparator: &tc.comparator, Threshold: &tc.threshold}
			if condition(r, tc.threshold, false) {
				t.Fatal("threshold itself fired")
			}
			if !condition(r, tc.firing, true) || condition(r, tc.recovered, true) {
				t.Fatal("incorrect recovery boundary")
			}
		})
	}
}

func probeFixture(t *testing.T, metric string, seconds int) *alertFixture {
	t.Helper()
	f := newFixture(t, seconds)
	comparator, threshold := ">", 200.0
	if metric == "probe_failed" {
		threshold = 0
	}
	if metric == "probe_tls_expiry_days" {
		comparator, threshold = "<", 200
	}
	raw, _ := json.Marshal(map[string]any{"metric": metric, "target": probeTarget, "comparator": comparator, "threshold": threshold})
	var err error
	f.rule, err = f.s.Save(context.Background(), f.rule.ID, patch(t, string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	f.probeValue(metric, true)
	return f
}

func (f *alertFixture) probeValue(metric string, firing bool) {
	p := v1.ProbeResult{Target: probeTarget, OK: true, LatencyMS: ptr(100.0), TLSExpiresAt: ptr(f.now.Add(250 * 24 * time.Hour))}
	if firing {
		switch metric {
		case "probe_failed":
			p.OK, p.LatencyMS = false, nil
		case "probe_latency_ms":
			p.LatencyMS = ptr(250.0)
		case "probe_tls_expiry_days":
			p.TLSExpiresAt = ptr(f.now.Add(150 * 24 * time.Hour))
		}
	}
	f.latest.report = probeLatest(f.now, p)
}

func TestProbeEvaluatorLifecycle(t *testing.T) {
	for _, metric := range []string{"probe_failed", "probe_latency_ms", "probe_tls_expiry_days"} {
		t.Run(metric, func(t *testing.T) {
			f := probeFixture(t, metric, 10)
			// all scope: a node without a matching result never starts pending.
			f.latest.report.ProbeResults = nil
			f.tick(t)
			if len(f.e.pending) != 0 || len(f.events(t, "all")) != 0 {
				t.Fatal("missing target fired")
			}
			f.probeValue(metric, true)
			f.tick(t)
			f.advance(5)
			f.latest.report.ProbeResults = nil
			f.tick(t)
			if len(f.e.pending) != 0 {
				t.Fatal("no data did not interrupt pending")
			}
			f.probeValue(metric, true)
			f.tick(t)
			f.advance(9)
			f.probeValue(metric, true)
			f.tick(t)
			if len(f.events(t, "all")) != 0 {
				t.Fatal("interrupted pending fired too early")
			}
			f.advance(1)
			f.probeValue(metric, true)
			f.tick(t)
			events := f.events(t, "firing")
			if len(events) != 1 || events[0].Target == nil || *events[0].Target != probeTarget {
				t.Fatalf("missing firing target: %+v", events)
			}
			detail, err := f.s.Event(t.Context(), events[0].ID)
			if err != nil || detail.Target == nil || *detail.Target != probeTarget {
				t.Fatalf("detail: %+v %v", detail, err)
			}
			summary, err := f.s.Summary(t.Context())
			if err != nil || len(summary.Newest) != 1 || summary.Newest[0].Target == nil {
				t.Fatalf("summary: %+v %v", summary, err)
			}
			f.latest.report.ProbeResults = nil
			f.tick(t)
			if len(f.events(t, "firing")) != 1 {
				t.Fatal("missing result resolved")
			}
			if metric == "probe_latency_ms" {
				f.latest.report.ProbeResults = []v1.ProbeResult{{Target: probeTarget, OK: false}}
				f.tick(t)
				if len(f.events(t, "firing")) != 1 {
					t.Fatal("failed latency resolved")
				}
			}
			f.advance(1)
			f.probeValue(metric, false)
			f.tick(t)
			if len(f.events(t, "resolved")) != 1 || len(f.events(t, "firing")) != 0 {
				t.Fatal("did not resolve")
			}
		})
	}
}

func TestProbeEvaluatorDisabledNeedsFreshReport(t *testing.T) {
	for _, metric := range []string{"probe_failed", "probe_latency_ms", "probe_tls_expiry_days"} {
		for _, mode := range []string{"removed", "disabled"} {
			t.Run(metric+"/"+mode, func(t *testing.T) {
				f := probeFixture(t, metric, 0)
				f.tick(t)
				if len(f.events(t, "firing")) != 1 {
					t.Fatal("did not fire")
				}
				if mode == "removed" {
					f.latest.report.Runtime.ProbeTargets = nil
				} else {
					f.latest.report.Runtime.DisabledFeatures = []string{v1.FeatureProbes}
				}
				f.latest.report.ReceivedAt = f.now.Add(-time.Hour)
				f.tick(t)
				if len(f.events(t, "firing")) != 1 {
					t.Fatal("stale configuration resolved")
				}
				f.latest.report.ReceivedAt = f.now
				f.tick(t)
				if len(f.events(t, "resolved")) != 1 {
					t.Fatal("fresh disabled configuration failed to resolve")
				}
			})
		}
	}
}

func TestProbeTargetChangeRestartsRound(t *testing.T) {
	f := probeFixture(t, "probe_failed", 10)
	original := definition(f.rule)
	f.tick(t)
	f.advance(10)
	f.probeValue("probe_failed", true)
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("did not fire")
	}
	changed := probeTarget + "/second"
	var err error
	f.rule, err = f.s.Save(t.Context(), f.rule.ID, patch(t, `{"target":"`+changed+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if definition(f.rule) == original {
		t.Fatal("target omitted from definition")
	}
	f.latest.report.Runtime.ProbeTargets = append(f.latest.report.Runtime.ProbeTargets, changed)
	f.latest.report.ProbeResults = append(f.latest.report.ProbeResults, v1.ProbeResult{Target: changed, OK: false})
	f.tick(t)
	if len(f.events(t, "resolved")) != 1 || len(f.events(t, "firing")) != 0 {
		t.Fatal("old event did not end")
	}
	oldEvent := f.events(t, "resolved")[0]
	if oldEvent.Target == nil || *oldEvent.Target != probeTarget {
		t.Fatal("target change rewrote historical snapshot")
	}
	f.advance(9)
	f.latest.report.ReceivedAt = f.now
	f.tick(t)
	if len(f.events(t, "firing")) != 0 {
		t.Fatal("new target reused pending")
	}
	f.advance(1)
	f.latest.report.ReceivedAt = f.now
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("new target did not fire after pending")
	}
}

func TestProbeRuleDefinitionAndDisabled(t *testing.T) {
	r := Rule{Metric: "probe_failed", Target: ptr(probeTarget)}
	original := definition(r)
	r.Target = ptr(probeTarget + "/other")
	if definition(r) == original {
		t.Fatal("target did not change definition")
	}
	for _, metric := range []string{"probe_failed", "probe_latency_ms", "probe_tls_expiry_days"} {
		r.Metric, r.Target = metric, ptr(probeTarget)
		runtime := v1.DefaultRuntimeSettings()
		runtime.ProbeTargets = []string{probeTarget}
		if metricDisabled(r, runtime) {
			t.Fatal("configured target disabled")
		}
		runtime.ProbeTargets = []string{probeTarget + "/other"}
		if !metricDisabled(r, runtime) {
			t.Fatal("removed target enabled")
		}
		runtime.ProbeTargets = []string{probeTarget}
		runtime.DisabledFeatures = []string{v1.FeatureProbes}
		if !metricDisabled(r, runtime) {
			t.Fatal("disabled probes enabled")
		}
	}
}

func TestProbeRulePersistence(t *testing.T) {
	f := newFixture(t, 0)
	raw := `{"name":"Probe","enabled":true,"scope_kind":"all","metric":"probe_failed","target":"` + probeTarget + `","comparator":">","threshold":0,"for_seconds":0,"severity":"warning"}`
	created, err := f.s.Save(t.Context(), "", patch(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := f.s.Get(t.Context(), created.ID)
	if err != nil || loaded.Target == nil || *loaded.Target != probeTarget {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	rules, err := f.s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rule := range rules {
		if rule.ID == created.ID {
			found = rule.Target != nil && *rule.Target == probeTarget
		}
	}
	if !found {
		t.Fatal("list omitted target")
	}
	loaded, err = f.s.Save(t.Context(), created.ID, patch(t, `{"name":"Renamed"}`))
	if err != nil || loaded.Target == nil || *loaded.Target != probeTarget {
		t.Fatalf("omitted target lost: %+v %v", loaded, err)
	}
	loaded, err = f.s.Save(t.Context(), created.ID, patch(t, `{"metric":"cpu_percent","target":null,"threshold":80}`))
	if err != nil || loaded.Target != nil {
		t.Fatalf("clear target: %+v %v", loaded, err)
	}
	loaded, err = f.s.Get(t.Context(), created.ID)
	if err != nil || loaded.Target != nil {
		t.Fatalf("clear target persisted: %+v %v", loaded, err)
	}
}

func TestProbeFailedLatencyInterruptsPending(t *testing.T) {
	f := probeFixture(t, "probe_latency_ms", 10)
	f.tick(t)
	f.advance(5)
	f.latest.report.ProbeResults = []v1.ProbeResult{{Target: probeTarget, OK: false}}
	f.tick(t)
	if len(f.e.pending) != 0 {
		t.Fatal("failed latency retained pending")
	}
	f.probeValue("probe_latency_ms", true)
	f.tick(t)
	f.advance(9)
	f.latest.report.ReceivedAt = f.now
	f.tick(t)
	if len(f.events(t, "all")) != 0 {
		t.Fatal("failed latency did not restart pending")
	}
	f.advance(1)
	f.latest.report.ReceivedAt = f.now
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("latency did not fire after new pending")
	}
}

func TestProbeExpiryPassingNowStaysFiring(t *testing.T) {
	f := probeFixture(t, "probe_tls_expiry_days", 0)
	expiry := f.now.Add(time.Second)
	f.latest.report.ProbeResults[0].TLSExpiresAt = &expiry
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("near expiry did not fire")
	}
	f.advance(2)
	f.latest.report.ReceivedAt = f.now
	f.tick(t)
	value := metricValue(f.latest.report, f.rule, f.now)
	if value == nil || *value != 0 || len(f.events(t, "firing")) != 1 {
		t.Fatal("expired TLS became no data or resolved")
	}
}
