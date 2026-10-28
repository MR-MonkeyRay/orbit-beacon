package alerts

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"orbitbeacon/internal/timekey"
)

func TestInboxTransitionsCommitWithoutOutboundCapacity(t *testing.T) {
	f := newFixture(t, 60)
	f.e.opts.Now = func() time.Time { return f.now }
	f.e.queue = make(chan Notification, 1)
	f.e.queue <- Notification{}
	f.tick(t)
	f.advance(60)
	f.value(90)
	f.tick(t)
	events := f.events(t, "firing")
	if len(events) != 1 {
		t.Fatalf("firing=%+v", events)
	}
	var created, details string
	if err := f.db.QueryRow(`SELECT created_at,details_json FROM inbox_notifications WHERE kind='alert' AND source_id=? AND transition='firing'`, events[0].ID).Scan(&created, &details); err != nil {
		t.Fatal(err)
	}
	if created != timekey.Format(f.now) {
		t.Fatalf("inbox created_at=%s not notification time", created)
	}
	var snapshot struct {
		RuleID     string    `json:"rule_id"`
		NodeID     string    `json:"node_id"`
		Transition string    `json:"transition"`
		StartedAt  time.Time `json:"started_at"`
	}
	if err := json.Unmarshal([]byte(details), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.RuleID != f.rule.ID || snapshot.NodeID != f.node || snapshot.Transition != "firing" || !snapshot.StartedAt.Equal(f.now.Add(-time.Minute)) {
		t.Fatalf("immutable snapshot=%+v", snapshot)
	}
	f.tick(t)
	f.e = NewEvaluator(f.s, f.latest, EvaluatorOptions{Now: func() time.Time { return f.now }, QueueSize: 1})
	f.e.queue <- Notification{}
	f.advance(1)
	f.value(10)
	f.tick(t)
	f.tick(t)
	var notifications int
	if err := f.db.QueryRow(`SELECT count(*) FROM inbox_notifications WHERE source_id=?`, events[0].ID).Scan(&notifications); err != nil || notifications != 2 {
		t.Fatalf("transitions=%d err=%v", notifications, err)
	}
	detail, err := f.s.Event(context.Background(), events[0].ID)
	if err != nil || detail.State != "resolved" {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	if _, err = f.s.Event(context.Background(), "not-found"); err != ErrNotFound {
		t.Fatalf("missing event=%v", err)
	}
}

func TestInboxFailureRollsBackAlertTransition(t *testing.T) {
	f := newFixture(t, 0)
	f.e.opts.Now = func() time.Time { return f.now }
	f.exec(t, `CREATE TRIGGER reject_inbox BEFORE INSERT ON inbox_notifications BEGIN SELECT RAISE(ABORT,'inbox unavailable'); END`)
	if err := f.e.Evaluate(context.Background(), f.now); err == nil {
		t.Fatal("inbox failure ignored")
	}
	if len(f.events(t, "all")) != 0 || len(f.e.queue) != 0 {
		t.Fatal("source state committed without inbox")
	}
	f.exec(t, `DROP TRIGGER reject_inbox`)
	f.tick(t)
	f.advance(1)
	f.value(10)
	f.exec(t, `CREATE TRIGGER reject_inbox BEFORE INSERT ON inbox_notifications BEGIN SELECT RAISE(ABORT,'inbox unavailable'); END`)
	if err := f.e.Evaluate(context.Background(), f.now); err == nil {
		t.Fatal("resolution inbox failure ignored")
	}
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("resolution committed without inbox")
	}
	f.exec(t, `DROP TRIGGER reject_inbox`)
	f.tick(t)
	if len(f.events(t, "resolved")) != 1 {
		t.Fatal("resolution did not recover after inbox restored")
	}
}
