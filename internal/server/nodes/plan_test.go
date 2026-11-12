package nodes_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/nodes"
)

func TestPlanPatchValidationAndReadOnlyGeo(t *testing.T) {
	s, db := newNodes(t, 10)
	n := pending(t, s, "edge")
	ctx := audit.WithSystemActor(context.Background(), "test")
	var patch nodes.MetadataPatch
	raw := `{"display_order":1000000,"price_amount":" 12.50 ","price_currency":" € ","billing_cycle":"year","auto_renew":true,"bandwidth":" 1 Gbps ","ipv4":true,"ipv6":true,"network_route":" transit ","plan_extra":" extra "}`
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		t.Fatal(err)
	}
	got, err := s.PatchMetadata(ctx, n.ID, patch)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayOrder != 1000000 || got.PriceAmount != "12.50" || got.PriceCurrency != "€" || !got.AutoRenew || !got.IPv4 || !got.IPv6 || got.Bandwidth != "1 Gbps" || got.NetworkRoute != "transit" || got.PlanExtra != "extra" {
		t.Fatalf("patch %+v", got)
	}
	var details string
	if err := db.QueryRow("SELECT details FROM audit_events WHERE action='node.metadata.patch' AND target_id=?", n.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var changed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(details), &changed); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"display_order", "price_amount", "price_currency", "billing_cycle", "auto_renew", "bandwidth", "ipv4", "ipv6", "network_route", "plan_extra"} {
		if _, ok := changed[key]; !ok {
			t.Fatalf("audit omitted %s: %s", key, details)
		}
	}
	for _, raw := range []string{`{"display_order":1000001}`, `{"display_order":-1000001}`, `{"price_amount":"-1"}`, `{"price_amount":"1.000"}`, `{"price_amount":"1e2"}`, `{"price_amount":"12345678901234567"}`, `{"price_currency":"US1"}`, `{"price_currency":"123456789"}`, `{"billing_cycle":"weekly"}`, `{"bandwidth":"bad\n"}`, `{"plan_extra":"` + strings.Repeat("x", 256) + `"}`, `{"network_route":"` + strings.Repeat("x", 129) + `"}`, `{"bandwidth":"` + strings.Repeat("x", 65) + `"}`} {
		patch = nodes.MetadataPatch{}
		if err := json.Unmarshal([]byte(raw), &patch); err != nil {
			t.Fatal(err)
		}
		if _, err = s.PatchMetadata(ctx, n.ID, patch); err == nil {
			t.Fatalf("accepted %s", raw)
		}
		after, err := s.Get(ctx, n.ID)
		if err != nil || !reflect.DeepEqual(after, got) {
			t.Fatal("invalid patch changed node")
		}
	}
	for _, raw := range []string{`{"remote_ip":"8.8.8.8"}`, `{"geo_country":"US"}`, `{"geo_location":"place"}`, `{"geo_checked_at":"now"}`, `{"plan_extra":"` + string([]byte{255}) + `"}`} {
		if json.Unmarshal([]byte(raw), &patch) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := db.Exec("UPDATE nodes SET remote_ip='8.8.8.8',geo_country='US',geo_location='California' WHERE id=?", n.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, n.ID)
	if err != nil || got.RemoteIP != "8.8.8.8" || got.GeoCountry != "US" || got.GeoLocation != "California" {
		t.Fatalf("geo %+v %v", got, err)
	}
}
