package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/services"
)

func TestServicesReuseOutboxAndEnabledChannels(t *testing.T) {
	f := newFixture(t)
	event := services.Event{ID: "00000000-0000-4000-8000-000000000101", ServiceID: "00000000-0000-4000-8000-000000000102", Name: "Health", Target: "https://internal.example/health", Transition: "down", Error: "timeout", OccurredAt: f.now}
	tx, e := beginMainWrite(t, f.db, context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if e = f.s.EnqueueService(context.Background(), tx, event); e != nil {
		t.Fatal(e)
	}
	if e = f.s.EnqueueService(context.Background(), tx, event); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	f.count(t, "SELECT count(*) FROM notification_deliveries", 1)
	var raw string
	if e = f.db.QueryRow("SELECT payload_json FROM notification_deliveries").Scan(&raw); e != nil {
		t.Fatal(e)
	}
	p, e := ParsePayload([]byte(raw))
	if e != nil || p.Service == nil || p.Service.Name != event.Name || p.Service.Target != event.Target || p.Service.Error != "timeout" {
		t.Fatalf("payload %s %v", raw, e)
	}
	text := renderMessage(p, "", 0)
	for _, value := range []string{event.Name, event.Target, event.Error} {
		if !strings.Contains(text, value) {
			t.Fatal(text)
		}
	}
	p.SummaryCount = 3
	text = renderMessage(p, "Custom {state}: {rule}", 0)
	for _, v := range []string{"Custom down: Health", "coalesced", event.Target, event.Error} {
		if !strings.Contains(text, v) {
			t.Fatal(text)
		}
	}
	encoded, e := json.Marshal(p)
	if e != nil || !strings.Contains(string(encoded), `"service"`) {
		t.Fatal(e)
	}
	if _, e = f.db.Exec("UPDATE notification_channels SET enabled=0"); e != nil {
		t.Fatal(e)
	}
	event.ID = "00000000-0000-4000-8000-000000000103"
	tx, e = beginMainWrite(t, f.db, context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if e = f.s.EnqueueService(context.Background(), tx, event); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	f.count(t, "SELECT count(*) FROM notification_deliveries", 1)
}

func TestServicesFinalEnqueueAfterDeliveryWorkerStops(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f.s.Run(ctx)
	event := services.Event{ID: "00000000-0000-4000-8000-000000000104", ServiceID: "00000000-0000-4000-8000-000000000102", Name: "Health", Target: "localhost", Transition: "down", Error: "timeout", OccurredAt: f.now}
	tx, e := beginMainWrite(t, f.db, t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if e = f.s.EnqueueService(t.Context(), tx, event); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	f.count(t, "SELECT count(*) FROM notification_deliveries", 1)
	shutdown, c := context.WithTimeout(t.Context(), time.Second)
	defer c()
	if e = f.s.Shutdown(shutdown); e != nil {
		t.Fatal(e)
	}
	tx, e = beginMainWrite(t, f.db, t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if e = f.s.EnqueueService(t.Context(), tx, event); e != ErrUnavailable {
		t.Fatalf("closed admission: %v", e)
	}
}
