package alerts

import (
	"context"
	"errors"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/validation"
)

func TestExpiryDaysUTCContinuous(t *testing.T) {
	for _, tc := range []struct {
		date, now string
		want      float64
	}{
		{"2026-10-27", "2026-10-27T00:00:00Z", 1},
		{"2026-10-27", "2026-10-27T12:00:00Z", .5},
		{"2026-10-27", "2026-10-27T18:00:00Z", .25},
		{"2026-10-27", "2026-10-28T00:00:00Z", 0},
		{"2026-10-27", "2026-10-29T00:00:00Z", 0},
		{"2026-10-28", "2026-10-27T18:00:00-06:00", 1},
	} {
		now, err := time.Parse(time.RFC3339, tc.now)
		if err != nil {
			t.Fatal(err)
		}
		got := expiryDays(&tc.date, now)
		if got == nil || *got != tc.want {
			t.Fatalf("%+v: got %v", tc, got)
		}
	}
	if expiryDays(nil, time.Now()) != nil || expiryDays(ptr("2026-02-30"), time.Now()) != nil {
		t.Fatal("missing/invalid date must have no value")
	}
}

func expiryFixture(t *testing.T, seconds int) *alertFixture {
	t.Helper()
	f := newFixture(t, seconds)
	var err error
	f.rule, err = f.s.Save(context.Background(), f.rule.ID, patch(t, `{"metric":"node_expiry_days","comparator":"<","threshold":30}`))
	if err != nil {
		t.Fatal(err)
	}
	f.latest.err = nodes.ErrNotFound
	return f
}

func TestExpiryMissingOfflineRenewalAndClear(t *testing.T) {
	for _, renewal := range []any{"2027-10-27", nil} {
		t.Run("renewal", func(t *testing.T) {
			f := expiryFixture(t, 0)
			f.tick(t)
			if len(f.events(t, "all")) != 0 {
				t.Fatal("missing date triggered < rule")
			}
			f.exec(t, "UPDATE nodes SET expires_on='2026-10-27',last_seen=NULL WHERE id=?", f.node)
			f.tick(t)
			events := f.events(t, "firing")
			if len(events) != 1 || *events[0].Value != .5 {
				t.Fatalf("offline expiry: %+v", events)
			}
			f.exec(t, "UPDATE nodes SET run_generation='2',report_session_id='restarted' WHERE id=?", f.node)
			f.latest.report.SessionID = "restarted"
			f.tick(t)
			if events = f.events(t, "all"); len(events) != 1 || events[0].ID == "" || events[0].State != "firing" {
				t.Fatalf("restart flapped: %+v", events)
			}
			f.exec(t, "UPDATE nodes SET expires_on=? WHERE id=?", renewal, f.node)
			f.tick(t)
			if len(f.events(t, "resolved")) != 1 {
				t.Fatal("renewal/removal did not recover")
			}
			f.tick(t)
			if len(f.events(t, "all")) != 1 {
				t.Fatal("missing/later date refired")
			}
		})
	}
}

func TestExpiryPendingSurvivesAgentRestartAndExpiredStaysFiring(t *testing.T) {
	f := expiryFixture(t, 30)
	f.exec(t, "UPDATE nodes SET expires_on='2026-10-26' WHERE id=?", f.node)
	f.tick(t)
	f.advance(29)
	f.exec(t, "UPDATE nodes SET run_generation='2',report_session_id='new' WHERE id=?", f.node)
	f.latest.report.SessionID = "new"
	f.tick(t)
	if len(f.events(t, "all")) != 0 {
		t.Fatal("fired before duration")
	}
	f.advance(1)
	f.tick(t)
	if len(f.events(t, "firing")) != 1 || *f.events(t, "firing")[0].Value != 0 {
		t.Fatal("restart reset expiry pending or expired value not zero")
	}
	f.advance(86400)
	f.tick(t)
	if len(f.events(t, "all")) != 1 || len(f.events(t, "firing")) != 1 {
		t.Fatal("expired event flapped")
	}
	f.exec(t, "UPDATE nodes SET status='disabled' WHERE id=?", f.node)
	f.tick(t)
	if len(f.events(t, "resolved")) != 1 {
		t.Fatal("disabled node did not resolve")
	}
}

func TestExpiryBoundsAndSQLiteConstraints(t *testing.T) {
	f := expiryFixture(t, 0)
	for _, raw := range []string{`{"threshold":0}`, `{"threshold":366}`, `{"comparator":">"}`} {
		if _, err := f.s.Save(context.Background(), f.rule.ID, patch(t, raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		} else {
			var fields *validation.Error
			if !errors.As(err, &fields) {
				t.Fatalf("expected field validation: %v", err)
			}
		}
	}
	for _, tc := range []struct {
		metric, comparator string
		threshold          float64
		valid              bool
	}{
		{"node_expiry_days", "<", 365, true}, {"node_expiry_days", "<", .5, true}, {"node_expiry_days", "<", 0, false},
		{"node_expiry_days", "<", 366, false}, {"node_expiry_days", ">", 30, false},
		{"traffic_quota_percent", ">", 1000, true}, {"load1", ">", 1000000, true}, {"cpu_percent", ">", 100, true},
	} {
		_, err := f.db.Exec("UPDATE alert_rules SET metric=?,comparator=?,threshold=? WHERE id=?", tc.metric, tc.comparator, tc.threshold, f.rule.ID)
		if (err == nil) != tc.valid {
			t.Fatalf("CHECK %+v: %v", tc, err)
		}
	}
	f.rule.Metric = "node_expiry_days"
	f.rule.Comparator = ptr("<")
	f.rule.Threshold = ptr(365.0)
	if !condition(f.rule, 366, true) || condition(f.rule, 373, true) {
		t.Fatal("expiry hysteresis clamped to percentage bounds")
	}
}

func TestExpiryIgnoresReportFreshnessAndReadErrors(t *testing.T) {
	for _, state := range []string{"missing", "stale", "error"} {
		t.Run(state, func(t *testing.T) {
			f := expiryFixture(t, 0)
			f.exec(t, "UPDATE nodes SET expires_on='2026-10-27' WHERE id=?", f.node)
			switch state {
			case "stale":
				f.latest.err = nil
				f.latest.report.ReceivedAt = f.now.Add(-24 * time.Hour)
			case "error":
				f.latest.err = errors.New("latest unavailable")
			}
			err := f.e.Evaluate(context.Background(), f.now)
			if state != "error" && err != nil {
				t.Fatal(err)
			}
			if events := f.events(t, "firing"); len(events) != 1 {
				t.Fatalf("expiry depends on %s report: %+v", state, events)
			}
			f.exec(t, "UPDATE nodes SET expires_on=NULL WHERE id=?", f.node)
			err = f.e.Evaluate(context.Background(), f.now)
			if state != "error" && err != nil {
				t.Fatal(err)
			}
			if len(f.events(t, "resolved")) != 1 {
				t.Fatal("missing date did not resolve independently of report")
			}
		})
	}
}

func TestExpiryFireRechecksDateAndIgnoresReportSession(t *testing.T) {
	for _, mutation := range []string{"renew", "clear", "restart"} {
		t.Run(mutation, func(t *testing.T) {
			f := expiryFixture(t, 0)
			f.exec(t, "UPDATE nodes SET expires_on='2026-10-27' WHERE id=?", f.node)
			inventory, err := f.e.readNodes(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "renew":
				f.exec(t, "UPDATE nodes SET expires_on='2027-10-27' WHERE id=?", f.node)
			case "clear":
				f.exec(t, "UPDATE nodes SET expires_on=NULL WHERE id=?", f.node)
			case "restart":
				f.exec(t, "UPDATE nodes SET run_generation='2',report_session_id='new' WHERE id=?", f.node)
			}
			value := expiryDays(inventory[0].expiresOn, f.now)
			if err := f.e.fire(context.Background(), f.rule, inventory[0], value, f.now); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mutation == "restart" {
				want = 1
			}
			if got := len(f.events(t, "firing")); got != want {
				t.Fatalf("%s: events=%d want=%d", mutation, got, want)
			}
		})
	}
}
