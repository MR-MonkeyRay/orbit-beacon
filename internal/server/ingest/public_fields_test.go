package ingest

import (
	"encoding/json"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
)

func TestPublicSelectedFieldsAbsentVersusNull(t *testing.T) {
	value := 42.0
	p := PublicNode{Name: "edge", Status: "unknown", Region: "California", Country: "US", Provider: "cloud", CPUPercent: &value, MetricStatus: unavailablePublicMetrics(), Fields: []string{}}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	json.Unmarshal(body, &got)
	if len(got) != 3 || string(got["name"]) != `"edge"` {
		t.Fatalf("private leaked %s", body)
	}
	p.Fields = []string{"region", "memory", "price", "expires_on", "billing_cycle", "ip_version", "traffic"}
	body, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(body, &got)
	for _, key := range []string{"price", "expires_on", "memory_percent"} {
		if string(got[key]) != "null" {
			t.Fatalf("selected missing %s: %s", key, body)
		}
	}
	for _, key := range []string{"cpu_percent", "provider", "remote_ip", "notes", "tags", "group_id"} {
		if _, ok := got[key]; ok {
			t.Fatalf("unselected %s: %s", key, body)
		}
	}
	if string(got["country"]) != `"US"` || string(got["metric_status"]) != `{"memory_percent":"unavailable"}` {
		t.Fatal(string(body))
	}
}
func TestPublicRegionFallbackAndOwnName(t *testing.T) {
	s := &Service{}
	record := nodes.PublicRecord{Name: "own", Region: "", Country: "US", GeoLocation: "California", Settings: nodes.PublicSettings{Fields: []string{"region", "price"}}, AvailabilityJSON: "[]"}
	got, err := s.ProjectPublicRecord(record, time.Now())
	if err != nil || got.Name != "own" || got.Region != "California" {
		t.Fatalf("projection %+v %v", got, err)
	}
	record.Region = "Manual"
	got, err = s.ProjectPublicRecord(record, time.Now())
	if err != nil || got.Region != "Manual" {
		t.Fatal("manual region lost")
	}
}
