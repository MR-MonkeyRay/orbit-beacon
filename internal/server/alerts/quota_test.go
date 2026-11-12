package alerts

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func quotaFixture(t *testing.T, seconds int) (*alertFixture, *map[string]float64, *error, *int) {
	t.Helper()
	f := newFixture(t, seconds)
	var err error
	f.rule, err = f.s.Save(context.Background(), f.rule.ID, patch(t, `{"metric":"traffic_quota_percent","threshold":80}`))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{f.node: 150}
	var readErr error
	calls := 0
	f.e = NewEvaluator(f.s, f.latest, EvaluatorOptions{Quota: func(context.Context, time.Time) (map[string]float64, error) { calls++; return values, readErr }})
	// 返回 map 指针，测试只修改键；读取器共享同一 map。
	return f, &values, &readErr, &calls
}

func TestQuotaFiresAbove100AndReadErrorNeverTransitions(t *testing.T) {
	f, values, readErr, calls := quotaFixture(t, 0)
	*readErr = errors.New("quota read failed")
	if err := f.e.Evaluate(context.Background(), f.now); !errors.Is(err, *readErr) {
		t.Fatalf("read error=%v", err)
	}
	if len(f.events(t, "all")) != 0 {
		t.Fatal("read error fired")
	}
	*readErr = nil
	f.tick(t)
	if len(f.events(t, "firing")) != 1 || *f.events(t, "firing")[0].Value != 150 {
		t.Fatal("quota did not fire over 100")
	}
	delete(*values, f.node)
	*readErr = errors.New("quota read failed")
	if err := f.e.Evaluate(context.Background(), f.now); !errors.Is(err, *readErr) {
		t.Fatal(err)
	}
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("read error resolved")
	}
	*readErr = nil
	f.tick(t)
	if len(f.events(t, "resolved")) != 1 {
		t.Fatal("quota removal did not resolve")
	}
	if *calls != 4 {
		t.Fatalf("reader calls=%d", *calls)
	}
}

func TestQuotaRolloverResolves(t *testing.T) {
	f, values, _, _ := quotaFixture(t, 0)
	f.tick(t)
	(*values)[f.node] = 0
	f.advance(30)
	f.tick(t)
	if len(f.events(t, "resolved")) != 1 {
		t.Fatal("rollover did not resolve")
	}
}

func TestQuotaIndependentOfReportFreshness(t *testing.T) {
	for _, state := range []string{"missing", "stale", "error"} {
		t.Run(state, func(t *testing.T) {
			f, _, _, _ := quotaFixture(t, 30)
			switch state {
			case "missing":
				f.latest.err = nodes.ErrNotFound
			case "stale":
				f.latest.report.ReceivedAt = f.now.Add(-time.Hour)
			case "error":
				f.latest.err = errors.New("report read failed")
			}
			tick := func() {
				t.Helper()
				err := f.e.Evaluate(context.Background(), f.now)
				if state != "error" && err != nil {
					t.Fatal(err)
				}
			}
			tick()
			f.advance(30)
			tick()
			if len(f.events(t, "firing")) != 1 {
				t.Fatal("offline/stale quota did not fire")
			}
			f.advance(3600)
			tick()
			if len(f.events(t, "firing")) != 1 {
				t.Fatal("offline/stale quota resolved")
			}
		})
	}
}

func TestQuotaDisabledFeaturesNeedFreshEvidenceToResolve(t *testing.T) {
	for _, feature := range []string{v1.FeatureTraffic, v1.FeatureNetwork} {
		t.Run(feature, func(t *testing.T) {
			f, _, _, _ := quotaFixture(t, 0)
			f.tick(t)
			f.latest.report.Metrics.MetricStatus = map[string]string{"traffic": "disabled"}
			f.latest.report.ReceivedAt = f.now.Add(-time.Hour)
			f.tick(t)
			if len(f.events(t, "firing")) != 1 {
				t.Fatal("stale disabled report resolved")
			}
			f.latest.report.ReceivedAt = f.now
			f.tick(t)
			if len(f.events(t, "resolved")) != 1 {
				t.Fatal("fresh disabled report did not resolve")
			}
			f.tick(t)
			if len(f.events(t, "all")) != 1 {
				t.Fatal("disabled feature fired")
			}
			f.latest.report.ReceivedAt = f.now.Add(-time.Hour)
			f.tick(t)
			if len(f.events(t, "all")) != 1 {
				t.Fatal("stale disabled feature fired")
			}
		})
	}
}

func TestQuotaReaderSkippedWithoutEnabledQuotaRule(t *testing.T) {
	f, _, _, calls := quotaFixture(t, 0)
	f.rule.Enabled = false
	if _, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"enabled":false}`)); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if *calls != 0 {
		t.Fatal("disabled rule read quota")
	}
	if _, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"enabled":true,"metric":"cpu_percent"}`)); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if *calls != 0 {
		t.Fatal("unrelated rule read quota")
	}
}

func TestQuotaComparatorsBoundsAndDuration(t *testing.T) {
	f, values, _, _ := quotaFixture(t, 30)
	for _, threshold := range []float64{0, -1, 1001} {
		r := f.rule
		r.Threshold = ptr(threshold)
		if ValidateRuleSyntax(r) == nil {
			t.Fatalf("accepted threshold %v", threshold)
		}
	}
	for _, threshold := range []float64{0.1, 1000} {
		r := f.rule
		r.Threshold = ptr(threshold)
		if err := ValidateRuleSyntax(r); err != nil {
			t.Fatal(err)
		}
	}
	f.rule.Comparator = ptr("<")
	f.rule.Threshold = ptr(150.0)
	if !condition(f.rule, 151, true) || condition(f.rule, 154, true) {
		t.Fatal("quota hysteresis clamped to 100")
	}
	f.tick(t)
	f.advance(29)
	f.tick(t)
	if len(f.events(t, "all")) != 0 {
		t.Fatal("quota fired early")
	}
	f.advance(1)
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("duration did not fire")
	}
	(*values)[f.node] = 70
	f.tick(t)
	if len(f.events(t, "resolved")) != 1 {
		t.Fatal("quota did not recover")
	}
}

func TestQuotaBelowRuleIgnoresNodesWithoutQuota(t *testing.T) {
	f, values, _, _ := quotaFixture(t, 0)
	var err error
	f.rule, err = f.s.Save(context.Background(), f.rule.ID, patch(t, `{"comparator":"<","threshold":50}`))
	if err != nil {
		t.Fatal(err)
	}
	delete(*values, f.node)
	f.tick(t)
	if len(f.events(t, "all")) != 0 {
		t.Fatal("missing quota treated as zero usage")
	}
}
