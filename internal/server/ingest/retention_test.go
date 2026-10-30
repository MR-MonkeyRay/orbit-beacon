package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/nodes"
)

func TestRetainedLastSampleLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newCoalesceFixture(t, 0)
	*f.report.Metrics.CPUPercent = 25
	*f.report.Metrics.UptimeSeconds = 3600
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	if _, err := acceptReport(ctx, f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	received := f.now
	for _, test := range []struct {
		name   string
		delta  time.Duration
		status string
		metric string
	}{
		{"fresh boundary", 45 * time.Second, "online", "ok"},
		{"source expired", 45*time.Second + time.Millisecond, "online", "stale"},
		{"online boundary", 180 * time.Second, "online", "stale"},
		{"agent offline", 180*time.Second + time.Nanosecond, "offline", "stale"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.now = received.Add(test.delta)
			node, err := publicNode(ctx, f.nodes, f.service, f.id, f.now)
			if err != nil || node.Status != test.status || node.CPUPercent == nil || *node.CPUPercent != 25 || node.UptimeSeconds == nil || *node.UptimeSeconds != 3600 {
				t.Fatalf("retained node=%+v err=%v", node, err)
			}
			assertPublicStatuses(t, node, test.metric, test.metric, test.metric, test.metric)
			assertPublicLastSeen(t, node, received)
			latest, err := f.service.Latest(ctx, f.id)
			if err != nil || latest.Metrics.MetricStatus["cpu_percent"] != "ok" || latest.Metrics.MetricStatus["uptime_seconds"] != "ok" || !latest.ReceivedAt.Equal(received) || *latest.Metrics.UptimeSeconds != 3600 {
				t.Fatalf("projection modified raw report=%+v err=%v", latest, err)
			}
		})
	}
	// A new Server process retains the asset and durable liveness, not numbers.
	records, _, err := f.nodes.ListPublicRecords(ctx, 100, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("durable page=%+v err=%v", records, err)
	}
	restarted := NewService(f.db, Options{Now: func() time.Time { return f.now }})
	node, err := publicNode(ctx, f.nodes, restarted, f.id, f.now)
	if err != nil || node.Status != "offline" || node.Name != "Coalesce" || node.CPUPercent != nil || node.UptimeSeconds != nil {
		t.Fatalf("restart node=%+v err=%v", node, err)
	}
	assertPublicStatuses(t, node, "unavailable", "unavailable", "unavailable", "unavailable")
	assertPublicLastSeen(t, node, records[0].LastSeen)
	// Reconnection replaces the whole sample, without advancing uptime locally.
	f.now = received.Add(181 * time.Second)
	f.report.Seq++
	*f.report.Metrics.CPUPercent = 50
	*f.report.Metrics.UptimeSeconds = 7200
	if _, err := acceptReport(ctx, f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	node, err = publicNode(ctx, f.nodes, f.service, f.id, f.now)
	if err != nil || node.Status != "online" || node.CPUPercent == nil || *node.CPUPercent != 50 || node.UptimeSeconds == nil || *node.UptimeSeconds != 7200 {
		t.Fatalf("reconnected node=%+v err=%v", node, err)
	}
	assertPublicStatuses(t, node, "ok", "ok", "ok", "ok")
	assertPublicLastSeen(t, node, f.now)
	// Explicit failure with a value is unavailable; never borrow an old success.
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.Metrics.MetricStatus["cpu_percent"] = "error"
	f.report.Metrics.MetricStatus["uptime_seconds"] = "stale"
	if _, err := acceptReport(ctx, f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	node, err = publicNode(ctx, f.nodes, f.service, f.id, f.now)
	if err != nil || node.CPUPercent != nil || node.UptimeSeconds == nil || *node.UptimeSeconds != 7200 {
		t.Fatalf("failed field reused old value=%+v err=%v", node, err)
	}
	assertPublicStatuses(t, node, "unavailable", "ok", "ok", "stale")
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.Metrics.UptimeSeconds = nil
	if _, err := acceptReport(ctx, f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	node, err = publicNode(ctx, f.nodes, f.service, f.id, f.now)
	if err != nil || node.UptimeSeconds != nil {
		t.Fatalf("missing field reused old value=%+v err=%v", node, err)
	}
	assertPublicStatuses(t, node, "unavailable", "ok", "ok", "unavailable")
}

func TestRetainedPublicSnapshotStillHonorsIdentityAndVisibility(t *testing.T) {
	for _, mutation := range []string{"withdraw", "disable", "delete", "revoke", "epoch", "credential", "generation", "session"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			f := newCoalesceFixture(t, 0)
			f.now = f.now.Add(181 * time.Second)
			switch mutation {
			case "withdraw":
				enabled := false
				if _, err := f.nodes.PatchPublicSettings(ctx, f.id, nodes.PublicSettingsPatch{Enabled: &enabled}); err != nil {
					t.Fatal(err)
				}
			case "disable":
				if err := f.nodes.SetDisabled(ctx, f.id, true); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if _, err := f.db.Exec("DELETE FROM nodes WHERE id=?", f.id); err != nil {
					t.Fatal(err)
				}
			case "revoke":
				if err := f.nodes.RevokeCredential(ctx, f.id); err != nil {
					t.Fatal(err)
				}
			default:
				updates := map[string]string{"epoch": "credential_epoch=credential_epoch+1", "credential": "token_hash=zeroblob(32)", "generation": "run_generation='2'", "session": "report_session_id='new-session'"}
				if _, err := f.db.Exec("UPDATE nodes SET "+updates[mutation]+" WHERE id=?", f.id); err != nil {
					t.Fatal(err)
				}
			}
			node, err := publicNode(ctx, f.nodes, f.service, f.id, f.now)
			if mutation == "withdraw" || mutation == "disable" || mutation == "delete" || mutation == "revoke" {
				if !errors.Is(err, nodes.ErrNotFound) {
					t.Fatalf("excluded node=%+v err=%v", node, err)
				}
			} else {
				if err != nil || node.CPUPercent != nil || node.UptimeSeconds != nil {
					t.Fatalf("old identity values=%+v err=%v", node, err)
				}
				assertPublicStatuses(t, node, "unavailable", "unavailable", "unavailable", "unavailable")
			}
			if mutation != "withdraw" {
				if latest, err := f.service.Latest(ctx, f.id); !errors.Is(err, nodes.ErrNotFound) {
					t.Fatalf("private old identity latest=%+v err=%v", latest, err)
				}
			}
		})
	}
}

func TestPublicRetentionNeverBackfillsHistory(t *testing.T) {
	ctx := context.Background()
	f := newCoalesceFixture(t, 0)
	samples := make(chan history.Sample, 4)
	f.service.samples = samples
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.Metrics.MetricStatus["cpu_percent"] = "stale"
	if _, err := acceptReport(ctx, f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	sample := <-samples
	if _, ok := sample.Values["cpu_percent"]; ok {
		t.Fatal("reported stale CPU entered history")
	}
	for _, elapsed := range []time.Duration{0, 45*time.Second + time.Millisecond, 181 * time.Second} {
		node, err := publicNode(ctx, f.nodes, f.service, f.id, f.now.Add(elapsed))
		if err != nil || node.CPUPercent == nil || *node.CPUPercent != 10 {
			t.Fatalf("retained stale CPU=%+v err=%v", node, err)
		}
		if len(samples) != 0 {
			t.Fatal("reading retained snapshot generated history")
		}
	}
}

func assertPublicLastSeen(t *testing.T, node PublicNode, want time.Time) {
	t.Helper()
	body, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		LastSeen *time.Time `json:"last_seen"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || wire.LastSeen == nil || !wire.LastSeen.Equal(want) {
		t.Fatalf("last_seen=%v want=%v err=%v body=%s", wire.LastSeen, want, err, body)
	}
}
