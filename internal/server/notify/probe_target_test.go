package notify

import (
	"encoding/json"
	"strings"
	"testing"

	"orbitbeacon/internal/server/alerts"
)

func TestProbeNotificationTarget(t *testing.T) {
	target := "https://private.example/health?key=token"
	for _, present := range []bool{false, true} {
		n := alerts.Notification{Rule: alerts.Rule{Name: "Probe", Metric: "probe_failed", Severity: "warning"}, State: "firing", NodeName: "Node"}
		if present {
			n.Rule.Target = &target
		}
		p := notificationPayload(n)
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParsePayload(raw)
		if err != nil {
			t.Fatal(err)
		}
		if (parsed.Rule.Target != nil) != present || present && *parsed.Rule.Target != target {
			t.Fatalf("payload target: %s", raw)
		}
		baseline := p
		baseline.Rule.Target = nil
		for _, count := range []int{0, 3} {
			p.SummaryCount, baseline.SummaryCount = count, count
			expected := renderMessage(baseline, "", 0)
			if present {
				expected += "\nTarget: " + target
			}
			if got := renderMessage(p, "", 0); got != expected {
				t.Fatalf("text=%q want=%q", got, expected)
			}
		}
		p.SummaryCount = 0
		expected := ""
		if present {
			expected = target
		}
		if got := renderMessage(p, "{target}", 0); got != expected {
			t.Fatalf("template=%q", got)
		}
		if !validTemplate("{target}") {
			t.Fatal("target template rejected")
		}
		if !present && strings.Contains(string(raw), `"target"`) {
			t.Fatalf("non-target payload changed: %s", raw)
		}
	}
}
