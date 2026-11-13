package services

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
)

func TestFailureThresholdObservations(t *testing.T) {
	for _, threshold := range []int{1, 2, 5, 10} {
		t.Run(fmt.Sprint(threshold), func(t *testing.T) {
			s := validService()
			s.FailureThreshold = threshold
			s.Notify = true
			var previous *Result
			for i := 1; i <= threshold+2; i++ {
				r, events := Advance(s, previous, Result{CheckedAt: time.Now()})
				want := "unknown"
				if i >= threshold {
					want = "down"
				}
				if r.Status != want || r.Failures != min(i, threshold) || len(events) != map[bool]int{true: 1, false: 0}[i == threshold] {
					t.Fatalf("check %d: %+v events=%v", i, r, events)
				}
				previous = &r
			}
			r, events := Advance(s, previous, Result{OK: true, CheckedAt: time.Now()})
			if r.Status != "up" || r.Failures != 0 || len(events) != 1 || events[0].Transition != "recovered" {
				t.Fatal(r, events)
			}
		})
	}
	s := validService()
	s.FailureThreshold = 5
	r := Result{Status: "up", Failures: 3, CheckedAt: time.Now()}
	s.FailureThreshold = 2
	if r.Status != "up" || r.Failures != 3 {
		t.Fatal("edit changed observation")
	}
	r, _ = Advance(s, &r, Result{CheckedAt: time.Now()})
	if r.Status != "down" || r.Failures != 2 {
		t.Fatal(r)
	}
	s.FailureThreshold = 10
	r, _ = Advance(s, &r, Result{CheckedAt: time.Now()})
	if r.Status != "down" || r.Failures != 3 {
		t.Fatal(r)
	}
	r, _ = Advance(s, &r, Result{OK: true, CheckedAt: time.Now()})
	if r.Status != "up" || r.Failures != 0 {
		t.Fatal(r)
	}
	s.FailureThreshold = 2
	r, _ = Advance(s, nil, Result{CheckedAt: time.Now()})
	s.FailureThreshold = 5
	r, _ = Advance(s, &r, Result{CheckedAt: time.Now()})
	if r.Status != "unknown" || r.Failures != 2 {
		t.Fatal(r)
	}
}

func TestFailureThresholdValidationAndFence(t *testing.T) {
	for _, n := range []int{-1, 0, 1, 2, 5, 10, 11} {
		s := validService()
		s.FailureThreshold = n
		if err := Validate(s); (err == nil) != (n >= 1 && n <= 10) {
			t.Fatalf("threshold %d: %v", n, err)
		}
		r := Result{Status: "unknown", CheckedAt: time.Now(), Failures: n}
		if err := ValidateResult(r); (err == nil) != (n >= 0 && n <= 10) {
			t.Fatalf("count %d: %v", n, err)
		}
	}
	for _, raw := range []string{`{"failure_threshold":null}`, `{"failure_threshold":1.5}`, `{"failure_threshold":"2"}`} {
		var p Patch
		if json.Unmarshal([]byte(raw), &p) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	s := validService()
	other := s
	other.FailureThreshold = 5
	if sameConfig(s, other) {
		t.Fatal("threshold omitted from fence")
	}
}

func TestFailureThresholdEditsPreserveObservations(t *testing.T) {
	m, _, _ := fixture(t, nil)
	ctx := t.Context()
	s := create(t, m)
	if s.FailureThreshold != 2 {
		t.Fatal(s)
	}
	s, err := m.Save(ctx, s.ID, Patch{FailureThreshold: ptr(5)})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err = m.Accept(ctx, s, Result{CheckedAt: m.now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err = m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = m.Save(ctx, s.ID, Patch{FailureThreshold: ptr(2)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(ctx, s.ID)
	if err != nil || got.Latest == nil || got.Latest.Failures != 3 || got.Latest.Status != "unknown" {
		t.Fatal(got, err)
	}
	if err = m.Accept(ctx, s, Result{CheckedAt: m.now()}); err != nil {
		t.Fatal(err)
	}
	s, err = m.Save(ctx, s.ID, Patch{FailureThreshold: ptr(10)})
	if err != nil {
		t.Fatal(err)
	}
	got, err = m.Get(ctx, s.ID)
	if err != nil || got.Latest.Status != "down" || got.Latest.Failures != 2 {
		t.Fatal(got, err)
	}
	if err = m.Accept(ctx, s, Result{CheckedAt: m.now()}); err != nil {
		t.Fatal(err)
	}
	got, err = m.Get(ctx, s.ID)
	if err != nil || got.Latest.Status != "down" || got.Latest.Failures != 3 {
		t.Fatal(got, err)
	}
	if err = m.Accept(ctx, s, Result{OK: true, CheckedAt: m.now()}); err != nil {
		t.Fatal(err)
	}
	got, err = m.Get(ctx, s.ID)
	if err != nil || got.Latest.Status != "up" {
		t.Fatal(got, err)
	}
}

func TestRecentHoursAndDetailStatistics(t *testing.T) {
	m, _, hist := fixture(t, nil)
	ctx := t.Context()
	now := time.Date(2026, 11, 11, 12, 17, 45, 0, time.FixedZone("offset", -7*3600))
	m.now = func() time.Time { return now }
	s := create(t, m)
	empty := create(t, m)
	start := now.UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	seed := func(at time.Time, checks, successes int, sum float64) {
		t.Helper()
		lease, err := hist.Acquire(ctx, storage.Writer)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		tx, err := lease.BeginWrite(lease.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		_, err = tx.ExecContext(lease.Context(), `INSERT INTO service_check_buckets VALUES(?,?,?,?,?,0,?)`, s.ID, at.Unix()/300*300, checks, successes, sum, sum)
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	seed(start, 3, 2, 20)
	seed(now.Add(-24*time.Hour).Truncate(5*time.Minute), 1, 0, 0) // in 24h availability, before strip
	seed(now.Add(-2*24*time.Hour), 2, 1, 10)
	seed(now.Add(-8*24*time.Hour), 4, 1, 10)
	seed(now.Add(-31*24*time.Hour), 1, 1, 10) // outside all windows
	seed(now.Add(time.Hour), 1, 1, 10)        // future data excluded
	if err := m.Accept(ctx, s, Result{OK: true, LatencyMS: ptr(30.0), CheckedAt: now}); err != nil {
		t.Fatal(err)
	}
	assert := func() {
		t.Helper()
		list, err := m.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got Service
		for _, v := range list {
			if v.ID == s.ID {
				got = v
			}
		}
		if len(got.RecentHours) != 24 || got.RecentHours[0] == nil || *got.RecentHours[0] != 66.67 || got.RecentHours[23] == nil || *got.RecentHours[23] != 100 {
			t.Fatalf("strip=%v", got.RecentHours)
		}
		for _, v := range got.RecentHours[1:23] {
			if v != nil {
				t.Fatal("nonempty gap")
			}
		}
		if got.Availability24H == nil || *got.Availability24H != 60 {
			t.Fatal(got.Availability24H)
		}
		raw, _ := json.Marshal(list)
		if strings.Contains(string(raw), `"stats"`) {
			t.Fatal(string(raw))
		}
		detail, err := m.Detail(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		stats := detail.Stats
		if stats.Availability24H == nil || *stats.Availability24H != 60 || *stats.Availability7D != 57.14 || *stats.Availability30D != 45.45 || *stats.AvgLatencyMS24H != 50.0/3 {
			t.Fatalf("stats=%+v", stats)
		}
		null, err := m.Detail(ctx, empty.ID)
		if err != nil || null.Stats != (Stats{}) {
			t.Fatal(null, err)
		}
		for _, v := range null.RecentHours {
			if v != nil {
				t.Fatal("empty strip")
			}
		}
	}
	assert()
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	assert()
	// At the next UTC hour boundary, the former live hour moves to index 22.
	now = now.UTC().Truncate(time.Hour).Add(time.Hour)
	list, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range list {
		if got.ID == s.ID && (got.RecentHours[0] != nil || got.RecentHours[22] == nil || *got.RecentHours[22] != 100 || got.RecentHours[23] != nil) {
			t.Fatal(got.RecentHours)
		}
	}
	if err = m.Accept(ctx, empty, Result{CheckedAt: now}); err != nil {
		t.Fatal(err)
	}
	failed, err := m.Detail(ctx, empty.ID)
	if err != nil || failed.Stats.Availability24H == nil || *failed.Stats.Availability24H != 0 || failed.Stats.AvgLatencyMS24H != nil {
		t.Fatal(failed.Stats, err)
	}
}

func TestBatchRequestValidation(t *testing.T) {
	id := "00000000-0000-4000-8000-000000000001"
	for _, r := range []BatchRequest{
		{Action: "pause"}, {ServiceIDs: []string{}, Action: "pause"}, {ServiceIDs: []string{id, id}, Action: "pause"},
		{ServiceIDs: []string{"bad"}, Action: "pause"}, {ServiceIDs: make([]string, 101), Action: "pause"},
		{ServiceIDs: []string{id}, Action: "delete"}, {ServiceIDs: []string{id}, Action: "bad"},
		{ServiceIDs: []string{id}, Action: "move"}, {ServiceIDs: []string{id}, Action: "move", GroupID: json.RawMessage(`""`)},
		{ServiceIDs: []string{id}, Action: "move", GroupID: json.RawMessage(`123`)}, {ServiceIDs: []string{id}, Action: "pause", GroupID: json.RawMessage(`null`)},
	} {
		if _, err := r.validate(); err == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
	for _, action := range []string{"pause", "resume", "move", "delete"} {
		r := BatchRequest{ServiceIDs: []string{id}, Action: action, Confirm: true}
		if action == "move" {
			r.GroupID = json.RawMessage(`null`)
		}
		if _, err := r.validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServicesBatchActionsAuditOrderAndSignal(t *testing.T) {
	m, db, _ := fixture(t, nil)
	ctx := audit.WithSystemActor(t.Context(), "test")
	a := create(t, m)
	b := create(t, m)
	group, err := m.SaveGroup(ctx, "", "group")
	if err != nil {
		t.Fatal(err)
	}
	missing := "00000000-0000-4000-8000-000000000999"
	signals := 0
	m.changed = func() { signals++ }
	for _, action := range []string{"pause", "pause", "resume", "move", "move", "delete"} {
		req := BatchRequest{ServiceIDs: []string{b.ID, missing, a.ID}, Action: action, Confirm: true}
		if action == "move" {
			req.GroupID = json.RawMessage(`"` + group.ID + `"`)
			if signals == 4 {
				req.GroupID = json.RawMessage(`null`)
			}
		}
		before := signals
		results, err := m.Batch(ctx, req)
		if err != nil || len(results) != 3 || signals != before+1 {
			t.Fatal(results, err, signals)
		}
		for i, id := range req.ServiceIDs {
			if results[i].ServiceID != id || (results[i].Status == "failed") != (id == missing) {
				t.Fatal(results)
			}
		}
		if results[1].Error.Code != "service_not_found" {
			t.Fatal(results)
		}
		if action != "delete" {
			s, err := m.Get(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if action == "pause" && s.Enabled || action == "resume" && !s.Enabled {
				t.Fatal(s)
			}
			if action == "move" && ((signals == 4 && (s.GroupID == nil || *s.GroupID != group.ID)) || (signals == 5 && s.GroupID != nil)) {
				t.Fatal(s)
			}
		}
	}
	lease, err := db.Acquire(ctx, storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	for _, id := range []string{a.ID, b.ID} {
		var updates, deletes int
		if err = lease.QueryRowContext(lease.Context(), "SELECT count(*) FROM audit_events WHERE target_id=? AND action='service.update'", id).Scan(&updates); err != nil {
			t.Fatal(err)
		}
		if err = lease.QueryRowContext(lease.Context(), "SELECT count(*) FROM audit_events WHERE target_id=? AND action='service.delete'", id).Scan(&deletes); err != nil || updates != 5 || deletes != 1 {
			t.Fatal(updates, deletes, err)
		}
	}
	if _, err = m.Batch(ctx, BatchRequest{ServiceIDs: []string{a.ID}, Action: "move", GroupID: json.RawMessage(`"` + missing + `"`)}); err == nil {
		t.Fatal("unknown group accepted")
	}
}

func TestPendingTotalsHourAlignmentAndWindowBounds(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 17, 45, 0, time.FixedZone("offset", -7*3600))
	hour := now.UTC().Truncate(time.Hour).Unix()
	m := New(nil, nil, func() time.Time { return now }, nil, nil, nil)
	m.buckets[bucketKey{"s", hour - 23*3600}] = aggregate{Checks: 3, Successes: 2, Sum: 20}
	m.buckets[bucketKey{"s", now.Unix() / 300 * 300}] = aggregate{Checks: 1, Successes: 1, Sum: 30}
	m.buckets[bucketKey{"s", hour - 24*3600}] = aggregate{Checks: 1}
	m.buckets[bucketKey{"s", hour + 3600}] = aggregate{Checks: 1, Successes: 1}
	totals, err := m.totalsBetween(t.Context(), now.Add(-24*time.Hour).Unix()/300*300, now.Unix()/300*300, 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	var recent [24]*float64
	for i := range recent {
		a := totals[bucketKey{"s", hour - int64(23-i)*3600}]
		recent[i] = percent(a.Successes, a.Checks)
	}
	if *recent[0] != 66.67 || *recent[23] != 100 {
		t.Fatal(recent)
	}
	for _, v := range recent[1:23] {
		if v != nil {
			t.Fatal("gap must be null")
		}
	}
	raw, _ := json.Marshal(recent)
	var decoded []any
	if err = json.Unmarshal(raw, &decoded); err != nil || len(decoded) != 24 {
		t.Fatal(string(raw), err)
	}
	if len(totals) != 2 {
		t.Fatal("window included past/future buckets", totals)
	}
	totals, err = m.totalsBetween(t.Context(), hour-23*3600, now.Unix()/300*300, 300, "s")
	a := totals[bucketKey{"s", 0}]
	if err != nil || a.Checks != 4 || a.Successes != 3 || a.Sum != 50 {
		t.Fatal(totals, err)
	}
	if got := percent(99999, 100000); got == nil || *got != 99.99 {
		t.Fatal(got)
	}
	if percent(0, 0) != nil {
		t.Fatal("no observations must be null")
	}
}
