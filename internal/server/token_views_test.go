package server

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/availability"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/traffic"
)

func assertTokenViewKeys(t *testing.T, value any, keys ...string) map[string]json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err = json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	actual := make([]string, 0, len(object))
	for k := range object {
		actual = append(actual, k)
	}
	slices.Sort(actual)
	slices.Sort(keys)
	if !reflect.DeepEqual(actual, keys) {
		t.Fatalf("keys=%v want %v", actual, keys)
	}
	return object
}

func TestAPITokenViewsHaveLiteralFieldSets(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	percent := 85.0
	n := nodes.Node{ID: "node", Name: "monitor", Notes: "private-notes", Tags: []string{"tag"}, CreatedAt: at}
	node := assertTokenViewKeys(t, tokenNodeDTO(n), "stopped", "id", "name", "status", "expires_on", "region", "provider", "group_id", "tags", "created_at", "last_seen", "presence", "agent_version")
	if string(node["name"]) != `"monitor"` {
		t.Fatal("lost node value")
	}
	assertTokenViewKeys(t, tokenProbeView{Status: "down", LatencyMS: &percent}, "status", "latency_ms", "status_code", "checked_at", "tls_expires_at", "uptime_24h", "checks_24h")
	eventKeys := []string{"id", "rule_id", "node_id", "rule_name", "metric", "severity", "node_name", "state", "value", "started_at", "resolved_at", "last_notified_at", "silenced"}
	target := "https://private-alert-target.example"
	event := alerts.Event{ID: "alert", RuleName: "probe", Metric: "probe_failed", Target: &target, StartedAt: at, Value: &percent}
	for _, value := range []any{tokenAlertDTO(event), tokenAlertPage{Events: tokenAlertDTOs([]alerts.Event{event})}, tokenAlertSummaryDTO(alerts.AlertSummary{Newest: []alerts.Event{event}})} {
		raw, err := json.Marshal(value)
		if err != nil || strings.Contains(string(raw), target) || strings.Contains(string(raw), `"target"`) {
			t.Fatalf("token target leak: %s %v", raw, err)
		}
	}
	assertTokenViewKeys(t, tokenAlertDTO(event), eventKeys...)
	assertTokenViewKeys(t, tokenAlertPage{Events: tokenAlertDTOs([]alerts.Event{event})}, "events", "has_more")
	assertTokenViewKeys(t, tokenAlertPage{NextCursor: "cursor"}, "events", "has_more", "next_cursor")
	assertTokenViewKeys(t, tokenAlertSummaryDTO(alerts.AlertSummary{Newest: []alerts.Event{event}}), "firing", "newest")
	assertTokenViewKeys(t, tokenFiringCounts{}, "total", "info", "warning", "critical", "unknown")
	response := tokenHistoryDTO(history.Response{Metric: "cpu_percent", Points: []history.Point{{T: at, Avg: 12, Max: 20}}})
	assertTokenViewKeys(t, response, "metric", "unit", "range", "step_seconds", "points")
	assertTokenViewKeys(t, response.Points[0], "t", "avg", "max")
	avail := tokenAvailabilityDTO(availability.Summary{Days: []availability.Daily{{Day: "2026-03-04", Online: 30, Percent: &percent}}})
	assertTokenViewKeys(t, avail, "days", "percent")
	assertTokenViewKeys(t, avail.Days[0], "day", "online_seconds", "offline_seconds", "unknown_seconds", "percent")
	network := tokenTrafficDTO(traffic.Summary{Daily: []traffic.Daily{{Day: "2026-03-04", RxBytes: "12", TxBytes: "34"}}})
	assertTokenViewKeys(t, network, "billing_mode", "billing_day", "quota_bytes", "cycle", "daily", "updated_at")
	assertTokenViewKeys(t, network.Cycle, "used_bytes", "start", "end", "rx_bytes", "tx_bytes", "total_bytes")
	assertTokenViewKeys(t, network.Daily[0], "day", "rx_bytes", "tx_bytes")
	assertTokenViewKeys(t, tokenGroupDTOs([]management.Group{{ID: "group", Name: "group-name"}})[0], "id", "name")
}

func TestAPITokenResponseSchemasHaveLiteralPropertiesAndValidReferences(t *testing.T) {
	data := readTokenOpenAPI(t)
	type schema = map[string]any
	schemas := data["components"].(map[string]any)["schemas"].(map[string]any)
	for name, keys := range map[string][]string{
		"APIToken":          {"id", "name", "created_by", "created_by_username", "created_at", "expires_at", "last_used_at", "rate_limit_per_minute"},
		"TokenNode":         {"stopped", "id", "name", "status", "expires_on", "region", "provider", "group_id", "tags", "created_at", "last_seen", "presence", "agent_version"},
		"TokenNodePage":     {"nodes", "has_more", "next_offset"},
		"TokenLatestSample": {"received_at", "sampled_at", "seq", "metrics", "server_time"},
		"TokenProbeSummary": {"probes"},
		"TokenProbeStatus":  {"status", "latency_ms", "status_code", "checked_at", "tls_expires_at", "uptime_24h", "checks_24h"},
	} {
		properties := schemas[name].(schema)["properties"].(schema)
		assertTokenViewKeys(t, properties, keys...)
	}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				if !tokenOpenAPIReferenceExists(data, ref) {
					t.Errorf("missing reference %s", ref)
				}
			}
			for _, nested := range v {
				walk(nested)
			}
		case []any:
			for _, nested := range v {
				walk(nested)
			}
		}
	}
	walk(data)
}

func readTokenOpenAPI(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}
func tokenOpenAPIReferenceExists(doc map[string]any, ref string) bool {
	if !strings.HasPrefix(ref, "#/") {
		return true
	}
	var value any = doc
	for _, part := range strings.Split(ref[2:], "/") {
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		value, ok = object[part]
		if !ok {
			return false
		}
	}
	return true
}
