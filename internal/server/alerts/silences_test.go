package alerts

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func addSilence(t *testing.T, f *alertFixture, kind string, id *string, start, end time.Time) Silence {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"reason": " maintenance ", "scope_kind": kind, "scope_id": id, "starts_at": start, "ends_at": end})
	var req SilenceRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	entry, err := f.s.CreateSilence(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}
func silenceCounts(t *testing.T, f *alertFixture, inbox int, notifications, wakes int) {
	t.Helper()
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM inbox_notifications").Scan(&count); err != nil || count != inbox {
		t.Fatalf("inbox=%d want %d: %v", count, inbox, err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM notification_deliveries").Scan(&count); err != nil || count != 0 {
		t.Fatalf("deliveries=%d: %v", count, err)
	}
	n := f.e.opts.Notifier.(*recordingNotifier)
	if len(n.notifications) != notifications || n.wakes != wakes {
		t.Fatalf("notifications=%+v wakes=%d", n.notifications, n.wakes)
	}
}
func silenceEvaluator(f *alertFixture, n *recordingNotifier) {
	f.e = NewEvaluator(f.s, f.latest, EvaluatorOptions{Now: func() time.Time { return f.now }, Notifier: n})
}
func TestSilenceTransitionsAndRestart(t *testing.T) {
	for _, scenario := range []string{"recover-inside", "recover-after-end", "expire", "delete", "restart-expire", "restart-recover", "before-window"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t, 0)
			n := &recordingNotifier{}
			silenceEvaluator(f, n)
			if scenario == "before-window" {
				f.tick(t)
			}
			s := addSilence(t, f, "all", nil, f.now, f.now.Add(time.Hour))
			f.tick(t)
			events := f.events(t, "firing")
			if len(events) != 1 || events[0].Silenced == (scenario == "before-window") {
				t.Fatalf("events=%+v", events)
			}
			if scenario == "before-window" {
				f.value(10)
				f.tick(t)
				silenceCounts(t, f, 2, 2, 2)
				if n.notifications[1].State != "resolved" {
					t.Fatal("missing recovery")
				}
				return
			}
			silenceCounts(t, f, 0, 0, 0)
			if scenario == "restart-expire" || scenario == "restart-recover" {
				silenceEvaluator(f, n)
			}
			if scenario == "recover-inside" || scenario == "restart-recover" || scenario == "recover-after-end" {
				if scenario == "recover-after-end" {
					f.advance(3600)
				}
				f.value(10)
				f.tick(t)
				silenceCounts(t, f, 0, 0, 0)
				if len(f.events(t, "resolved")) != 1 {
					t.Fatal("missing resolved event")
				}
				return
			}
			if scenario == "delete" {
				if err := f.s.DeleteSilence(context.Background(), s.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				f.advance(3600)
			}
			f.value(90)
			f.tick(t)
			f.tick(t)
			silenceCounts(t, f, 1, 1, 1)
			if f.events(t, "firing")[0].Silenced || n.notifications[0].State != "firing" {
				t.Fatal("missing release")
			}
			f.value(10)
			f.tick(t)
			f.tick(t)
			silenceCounts(t, f, 2, 2, 2)
			if n.notifications[1].State != "resolved" {
				t.Fatal("missing recovery after release")
			}
		})
	}
}
func TestSilenceReleaseAtomicAndFreshRecoveryFlag(t *testing.T) {
	f := newFixture(t, 0)
	n := &recordingNotifier{}
	silenceEvaluator(f, n)
	addSilence(t, f, "all", nil, f.now, f.now.Add(time.Hour))
	f.tick(t)
	old, err := f.e.readOpen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.advance(3600)
	f.value(90)
	f.exec(t, `CREATE TRIGGER fail_silence_inbox BEFORE INSERT ON inbox_notifications BEGIN SELECT RAISE(ABORT,'inbox failed'); END`)
	if err := f.e.Evaluate(context.Background(), f.now); err == nil {
		t.Fatal("expected release failure")
	}
	if !f.events(t, "firing")[0].Silenced {
		t.Fatal("flag committed after failed inbox")
	}
	silenceCounts(t, f, 0, 0, 0)
	f.exec(t, "DROP TRIGGER fail_silence_inbox")
	f.tick(t)
	// old still has silenced=true; resolve must read the committed flag.
	for _, o := range old {
		if err := f.e.resolve(context.Background(), o, ptr(10.0), f.now); err != nil {
			t.Fatal(err)
		}
	}
	silenceCounts(t, f, 2, 2, 2)
}
func TestSilenceQueryFailureDoesNotChangeEvent(t *testing.T) {
	f := newFixture(t, 0)
	n := &recordingNotifier{}
	silenceEvaluator(f, n)
	addSilence(t, f, "all", nil, f.now, f.now.Add(time.Hour))
	f.tick(t)
	f.exec(t, "ALTER TABLE alert_silences RENAME TO unavailable_silences")
	f.advance(3600)
	f.value(90)
	if err := f.e.Evaluate(context.Background(), f.now); err == nil {
		t.Fatal("query error ignored")
	}
	if !f.events(t, "firing")[0].Silenced {
		t.Fatal("query failure changed flag")
	}
	silenceCounts(t, f, 0, 0, 0)
	f.exec(t, "ALTER TABLE unavailable_silences RENAME TO alert_silences")
	f.tick(t)
	silenceCounts(t, f, 1, 1, 1)
}
func TestSilenceScopeCurrentGroupAndBoundaries(t *testing.T) {
	const group = "00000000-0000-4000-8000-000000000002"
	const other = "00000000-0000-4000-8000-000000000003"
	for _, scenario := range []string{"all", "node", "group", "other-node", "other-group", "move-in", "move-out", "start", "end"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t, 0)
			n := &recordingNotifier{}
			silenceEvaluator(f, n)
			f.exec(t, "INSERT INTO groups(id,name) VALUES(?,'g'),(?,'other')", group, other)
			f.exec(t, "INSERT INTO nodes(id,name,status,created_at) VALUES(?,'other','pending',?)", other, f.now.Format(time.RFC3339Nano))
			f.exec(t, "UPDATE nodes SET group_id=? WHERE id=?", group, f.node)
			kind := "all"
			var id *string
			start := f.now
			end := f.now.Add(time.Hour)
			want := true
			switch scenario {
			case "node":
				kind = "node"
				id = &f.node
			case "group", "move-in", "move-out":
				kind = "group"
				id = ptr(group)
			case "other-node":
				kind = "node"
				id = ptr(other)
				want = false
			case "other-group":
				kind = "group"
				id = ptr(other)
				want = false
			case "end":
				start = f.now.Add(-time.Hour)
				end = f.now.Add(time.Second)
				want = false
			}
			if scenario == "move-in" {
				f.exec(t, "UPDATE nodes SET group_id=? WHERE id=?", other, f.node)
			}
			addSilence(t, f, kind, id, start, end)
			if scenario == "move-in" {
				f.exec(t, "UPDATE nodes SET group_id=? WHERE id=?", group, f.node)
			}
			if scenario == "move-out" {
				f.exec(t, "UPDATE nodes SET group_id=? WHERE id=?", other, f.node)
				want = false
			}
			if scenario == "end" {
				f.advance(1)
				f.value(90)
			}
			f.tick(t)
			events := f.events(t, "firing")
			if len(events) != 1 || events[0].Silenced != want {
				t.Fatalf("events=%+v want silenced=%v", events, want)
			}
		})
	}
}

func TestSilenceUsesTriggerTimeAndTransactionCurrentScope(t *testing.T) {
	f := newFixture(t, 60)
	n := &recordingNotifier{}
	silenceEvaluator(f, n)
	f.tick(t)
	f.advance(30)
	addSilence(t, f, "all", nil, f.now, f.now.Add(time.Hour))
	f.advance(30)
	f.value(90)
	f.tick(t)
	if events := f.events(t, "firing"); len(events) != 1 || !events[0].Silenced || !events[0].StartedAt.Equal(f.now.Add(-time.Minute)) {
		t.Fatalf("events=%+v", events)
	}
	silenceCounts(t, f, 0, 0, 0)
}

func TestSilenceResolveReadsFlagInsideTransaction(t *testing.T) {
	f := newFixture(t, 0)
	n := &recordingNotifier{}
	silenceEvaluator(f, n)
	addSilence(t, f, "all", nil, f.now, f.now.Add(time.Hour))
	f.tick(t)
	open, err := f.e.readOpen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range open {
		// Deliberately stale input: persisted silenced=true must suppress recovery.
		o.Silenced = false
		if err := f.e.resolve(context.Background(), o, ptr(10.0), f.now); err != nil {
			t.Fatal(err)
		}
	}
	silenceCounts(t, f, 0, 0, 0)
}

func TestSilenceOverlappingWindowsAndCurrentGroupRelease(t *testing.T) {
	f := newFixture(t, 0)
	n := &recordingNotifier{}
	silenceEvaluator(f, n)
	group := "00000000-0000-4000-8000-000000000002"
	f.exec(t, "INSERT INTO groups(id,name) VALUES(?,'group')", group)
	f.exec(t, "UPDATE nodes SET group_id=? WHERE id=?", group, f.node)
	all := addSilence(t, f, "all", nil, f.now, f.now.Add(time.Hour))
	addSilence(t, f, "group", &group, f.now, f.now.Add(2*time.Hour))
	f.tick(t)
	if err := f.s.DeleteSilence(context.Background(), all.ID); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	silenceCounts(t, f, 0, 0, 0)
	f.exec(t, "UPDATE nodes SET group_id=NULL WHERE id=?", f.node)
	f.tick(t)
	f.tick(t)
	silenceCounts(t, f, 1, 1, 1)
}

func TestSilenceConstructorsDoNotReadClock(t *testing.T) {
	calls := 0
	clock := func() time.Time { calls++; return time.Now() }
	service := NewService(nil, clock)
	NewEvaluator(service, nil, EvaluatorOptions{Now: clock})
	if calls != 0 {
		t.Fatalf("constructor clock calls=%d", calls)
	}
}

func TestSilenceFireSeesWindowAndGroupAfterInventoryRead(t *testing.T) {
	f := newFixture(t, 0)
	n := &recordingNotifier{}
	silenceEvaluator(f, n)
	inventory, err := f.e.readNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	group := "00000000-0000-4000-8000-000000000002"
	f.exec(t, "INSERT INTO groups(id,name) VALUES(?,'new group')", group)
	addSilence(t, f, "group", &group, f.now, f.now.Add(time.Hour))
	f.exec(t, "UPDATE nodes SET group_id=? WHERE id=?", group, f.node)
	if err = f.e.fire(context.Background(), f.rule, inventory[0], ptr(90.0), f.now, f.now); err != nil {
		t.Fatal(err)
	}
	if events := f.events(t, "firing"); len(events) != 1 || !events[0].Silenced {
		t.Fatalf("events=%+v", events)
	}
	silenceCounts(t, f, 0, 0, 0)
}

// 窗口生效时不能进入写路径：全局静默时逐事件写事务会与上报争抢写锁。
func TestReleasableSilencesOnlyReturnsEndedWindows(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 0)
	silenceEvaluator(f, &recordingNotifier{})
	s := addSilence(t, f, "all", nil, f.now, f.now.Add(time.Hour))
	f.tick(t)
	open, err := f.e.readOpen(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("open=%+v err=%v", open, err)
	}
	if got, err := f.e.releasableSilences(ctx, open, f.now); err != nil || len(got) != 0 {
		t.Fatalf("active window released %+v: %v", got, err)
	}
	if err := f.s.DeleteSilence(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := f.e.releasableSilences(ctx, open, f.now); err != nil || len(got) != 1 {
		t.Fatalf("ended window not released %+v: %v", got, err)
	}
	for key, o := range open {
		f.e.resolutions[key] = pendingResolution{eventID: o.ID}
	}
	if got, err := f.e.releasableSilences(ctx, open, f.now); err != nil || len(got) != 0 {
		t.Fatalf("event awaiting recovery released %+v: %v", got, err)
	}
}

// 预筛之后状态可能已变，写事务内的条件才是最后的判定。
func TestReleaseSilenceRechecksInsideTransaction(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 0)
	n := &recordingNotifier{}
	silenceEvaluator(f, n)
	s := addSilence(t, f, "all", nil, f.now, f.now.Add(time.Hour))
	f.tick(t)
	open, err := f.e.readOpen(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("open=%+v err=%v", open, err)
	}
	var event openEvent
	for _, o := range open {
		event = o
	}
	if err := f.e.releaseSilence(ctx, event, f.now); err != nil {
		t.Fatal(err)
	}
	if len(n.notifications) != 0 || !f.events(t, "firing")[0].Silenced {
		t.Fatalf("window created after the read was ignored: %+v", n.notifications)
	}
	if err := f.s.DeleteSilence(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.e.releaseSilence(ctx, event, f.now); err != nil {
			t.Fatal(err)
		}
	}
	if len(n.notifications) != 1 || n.notifications[0].State != "firing" || n.wakes != 1 {
		t.Fatalf("notifications=%+v wakes=%d", n.notifications, n.wakes)
	}
}
