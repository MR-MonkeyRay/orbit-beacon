package services

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func TestMonitorSilenceIncidentTransitions(t *testing.T) {
	for _, incident := range []string{"down", "latency"} {
		for _, scenario := range []string{"recover-inside", "recover-after-end", "release", "pre-silence", "notify-false"} {
			t.Run(incident+"/"+scenario, func(t *testing.T) {
				s := validService()
				s.Notify = true
				s.FailureThreshold = 1
				s.LatencyThresholdMS = 50
				at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
				var previous *Result
				step := func(active, unhealthy bool, want ...string) {
					t.Helper()
					r := Result{OK: true, LatencyMS: ptr(10.0), CheckedAt: at}
					if unhealthy {
						if incident == "down" {
							r.OK = false
							r.Error = "timeout"
						} else {
							r.LatencyMS = ptr(100.0)
						}
					}
					result, events := advanceSilenced(s, previous, r, active)
					if len(events) != len(want) {
						t.Fatalf("events=%+v want=%v result=%+v", events, want, result)
					}
					for i, e := range events {
						if e.Transition != want[i] || !e.OccurredAt.Equal(at) {
							t.Fatalf("event=%+v want=%s", e, want[i])
						}
					}
					raw, err := json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
					previous = &Result{}
					if err = json.Unmarshal(raw, previous); err != nil {
						t.Fatal(err)
					}
					if err = ValidateResult(*previous); err != nil {
						t.Fatal(err)
					}
					at = at.Add(time.Minute)
				}
				firing, recovery := "down", "recovered"
				if incident == "latency" {
					firing, recovery = "latency_high", "latency_recovered"
				}
				if scenario == "notify-false" {
					s.Notify = false
				}
				active := scenario != "pre-silence"
				if incident == "latency" {
					step(active, true)
					step(active, true)
				}
				if scenario == "pre-silence" {
					step(false, true, firing)
					step(true, false, recovery)
					return
				}
				step(true, true)
				if previous.DownSilenced != (incident == "down" && s.Notify) || previous.LatencySilenced != (incident == "latency" && s.Notify) {
					t.Fatalf("flags=%+v", previous)
				}
				switch scenario {
				case "release":
					step(false, true, firing)
					step(false, true)
					step(true, false, recovery)
				case "recover-inside":
					step(true, false)
					step(false, false)
				case "recover-after-end":
					step(false, false)
					step(false, false)
				case "notify-false":
					step(false, true)
					step(false, false)
				}
			})
		}
	}
}

func TestMonitorSilenceCertificateBudget(t *testing.T) {
	s := validService()
	s.Notify = true
	at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	expiry := at.Add(24 * time.Hour)
	r, events := advanceSilenced(s, nil, Result{OK: true, CheckedAt: at, TLSExpiresAt: &expiry}, true)
	if len(events) != 0 || r.CertificateNotifiedDay != "" {
		t.Fatalf("silenced certificate=%+v events=%+v", r, events)
	}
	r, events = advanceSilenced(s, &r, Result{OK: true, CheckedAt: at.Add(time.Minute), TLSExpiresAt: &expiry}, false)
	if len(events) != 1 || events[0].Transition != "certificate_expiring" || r.CertificateNotifiedDay != "2026-11-11" {
		t.Fatalf("certificate=%+v events=%+v", r, events)
	}
	_, events = advanceSilenced(s, &r, Result{OK: true, CheckedAt: at.Add(2 * time.Minute), TLSExpiresAt: &expiry}, false)
	if len(events) != 0 {
		t.Fatal(events)
	}
}

func TestMonitorSilenceResultValidation(t *testing.T) {
	r := Result{Status: "up", CheckedAt: time.Now(), DownSilenced: true}
	if ValidateResult(r) == nil {
		t.Fatal("closed down accepted")
	}
	r.DownSilenced = false
	r.LatencySilenced = true
	if ValidateResult(r) == nil {
		t.Fatal("closed latency accepted")
	}
	var legacy Result
	if err := json.Unmarshal([]byte(`{"status":"up","checked_at":"2026-11-11T12:00:00Z"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResult(legacy); err != nil {
		t.Fatal(err)
	}
}

func silenceExec(t *testing.T, db *storage.Database, query string, args ...any) (sql.Result, error) {
	t.Helper()
	l, err := db.Acquire(t.Context(), storage.Writer)
	if err != nil {
		return nil, err
	}
	defer l.Close()
	tx, err := l.BeginWrite(l.Context())
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(l.Context(), query, args...)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}
func silenceScan(t *testing.T, db *storage.Database, query string, dest ...any) {
	t.Helper()
	l, err := db.Acquire(t.Context(), storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = l.QueryRowContext(l.Context(), query).Scan(dest...); err != nil {
		t.Fatal(err)
	}
}
func monitorSilence(t *testing.T, m *Manager, kind string, id any, start, end time.Time) {
	t.Helper()
	if _, err := silenceExec(t, m.db, `INSERT INTO alert_silences(id,reason,scope_kind,scope_id,starts_at,ends_at,created_at) VALUES('silence','maintenance',?,?,?,?,?)`, kind, id, timekey.Format(start), timekey.Format(end), timekey.Format(start)); err != nil {
		t.Fatal(err)
	}
}
func monitorCheck(t *testing.T, m *Manager, s Service, ok bool, at time.Time, expiry *time.Time) {
	t.Helper()
	if err := m.Accept(t.Context(), s, Result{OK: ok, CheckedAt: at, TLSExpiresAt: expiry, Error: map[bool]string{true: "", false: "timeout"}[ok]}); err != nil {
		t.Fatal(err)
	}
}
func monitorCounts(t *testing.T, m *Manager, n *recordingNotifier, want int) {
	t.Helper()
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	silenceScan(t, m.db, "SELECT count(*) FROM inbox_notifications", &count)
	if count != want || len(n.events) != want {
		t.Fatalf("inbox=%d channels=%+v want=%d", count, n.events, want)
	}
}

func TestMonitorSilenceStoreScopesAndRestart(t *testing.T) {
	for _, scope := range []string{"all", "service_group", "other-service-group", "group", "node", "future", "end"} {
		t.Run(scope, func(t *testing.T) {
			n := &recordingNotifier{}
			m, db, hist := fixture(t, n)
			g, err := m.SaveGroup(t.Context(), "", "monitors")
			if err != nil {
				t.Fatal(err)
			}
			other, err := m.SaveGroup(t.Context(), "", "other")
			if err != nil {
				t.Fatal(err)
			}
			s := create(t, m)
			raw, _ := json.Marshal(g.ID)
			s, err = m.Save(t.Context(), s.ID, Patch{GroupID: raw, FailureThreshold: ptr(1)})
			if err != nil {
				t.Fatal(err)
			}
			at := m.now()
			kind := scope
			var id any
			start, end := at, at.Add(time.Hour)
			switch scope {
			case "service_group":
				id = g.ID
			case "other-service-group":
				kind = "service_group"
				id = other.ID
			case "group":
				id = g.ID
				if _, err = silenceExec(t, db, "INSERT INTO groups(id,name) VALUES(?,'nodes')", id); err != nil {
					t.Fatal(err)
				}
			case "node":
				id = g.ID
				if _, err = silenceExec(t, db, "INSERT INTO nodes(id,name,created_at) VALUES(?,'node',?)", id, timekey.Format(at)); err != nil {
					t.Fatal(err)
				}
			case "future":
				kind = "all"
				start = at.Add(time.Minute)
			case "end":
				kind = "all"
				start = at.Add(-time.Hour)
				end = at
			}
			monitorSilence(t, m, kind, id, start, end)
			m.now = func() time.Time { return at.Add(2 * time.Hour) } // Event time, not wall/flush time.
			monitorCheck(t, m, s, false, at, nil)
			if scope != "all" && scope != "service_group" {
				monitorCounts(t, m, n, 1)
				monitorCheck(t, m, s, true, at.Add(time.Second), nil)
				monitorCounts(t, m, n, 2)
				return
			}
			monitorCounts(t, m, n, 0)
			m = New(db, hist, m.now, nil, n, nil)
			monitorCheck(t, m, s, false, end, nil)
			monitorCounts(t, m, n, 1)
			monitorCheck(t, m, s, false, end.Add(time.Second), nil)
			monitorCounts(t, m, n, 1)
			monitorCheck(t, m, s, true, end.Add(2*time.Second), nil)
			monitorCounts(t, m, n, 2)
			if n.events[0].Transition != "down" || n.events[1].Transition != "recovered" {
				t.Fatal(n.events)
			}
		})
	}
}

func TestMonitorSilencedUntilCurrentMembership(t *testing.T) {
	m, db, _ := fixture(t, nil)
	g, err := m.SaveGroup(t.Context(), "", "monitors")
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.SaveGroup(t.Context(), "", "other")
	if err != nil {
		t.Fatal(err)
	}
	s := create(t, m)
	raw, _ := json.Marshal(g.ID)
	s, err = m.Save(t.Context(), s.ID, Patch{GroupID: raw})
	if err != nil {
		t.Fatal(err)
	}
	at := m.now()
	assertUntil := func(want *time.Time) {
		t.Helper()
		list, err := m.List(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		detail, err := m.Detail(t.Context(), s.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range []*time.Time{list[0].SilencedUntil, detail.SilencedUntil} {
			if (got == nil) != (want == nil) || got != nil && !got.Equal(*want) {
				t.Fatalf("until=%v want=%v", got, want)
			}
		}
	}
	add := func(id, kind string, group any, start, end time.Time) {
		t.Helper()
		if _, err := silenceExec(t, db, `INSERT INTO alert_silences(id,reason,scope_kind,scope_id,starts_at,ends_at,created_at) VALUES(?,'maintenance',?,?,?,?,?)`, id, kind, group, timekey.Format(start), timekey.Format(end), timekey.Format(at)); err != nil {
			t.Fatal(err)
		}
	}
	assertUntil(nil)
	add("group", "service_group", g.ID, at, at.Add(2*time.Hour))
	assertUntil(ptr(at.Add(2 * time.Hour)))
	add("all", "all", nil, at, at.Add(time.Hour))
	assertUntil(ptr(at.Add(2 * time.Hour)))
	add("later", "all", nil, at, at.Add(3*time.Hour))
	assertUntil(ptr(at.Add(3 * time.Hour)))
	add("future", "all", nil, at.Add(time.Hour), at.Add(4*time.Hour))
	assertUntil(ptr(at.Add(3 * time.Hour)))
	if _, err = silenceExec(t, db, "DELETE FROM alert_silences WHERE id IN ('later','future')"); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(other.ID)
	s, err = m.Save(t.Context(), s.ID, Patch{GroupID: raw})
	if err != nil {
		t.Fatal(err)
	}
	assertUntil(ptr(at.Add(time.Hour)))
	m.now = func() time.Time { return at.Add(time.Hour) }
	assertUntil(nil)
}

func TestMonitorSilenceFlushRollback(t *testing.T) {
	n := &recordingNotifier{fail: true}
	m, db, hist := fixture(t, n)
	s := create(t, m)
	s, err := m.Save(t.Context(), s.ID, Patch{FailureThreshold: ptr(1)})
	if err != nil {
		t.Fatal(err)
	}
	at := m.now()
	monitorSilence(t, m, "all", nil, at, at.Add(time.Hour))
	monitorCheck(t, m, s, false, at, nil)
	monitorCounts(t, m, n, 0)
	if _, err = silenceExec(t, db, "DELETE FROM alert_silences"); err != nil {
		t.Fatal(err)
	}
	monitorCheck(t, m, s, false, at.Add(time.Minute), nil)
	if err = m.Flush(t.Context()); err == nil {
		t.Fatal("enqueue failure ignored")
	}
	var raw string
	var persisted Result
	var count int
	silenceScan(t, db, "SELECT result_json FROM service_checks_latest", &raw)
	if err = json.Unmarshal([]byte(raw), &persisted); err != nil || !persisted.DownSilenced {
		t.Fatalf("state=%s err=%v", raw, err)
	}
	silenceScan(t, db, "SELECT count(*) FROM inbox_notifications", &count)
	if count != 0 {
		t.Fatal(count)
	}
	m = New(db, hist, m.now, nil, n, nil)
	n.fail = false
	monitorCheck(t, m, s, false, at.Add(2*time.Minute), nil)
	monitorCounts(t, m, n, 1)
}

func TestMonitorSilenceBufferedRecoveryAndCertificate(t *testing.T) {
	for _, scenario := range []string{"recovery-inside", "recovery-after-end", "pre-silence", "certificate", "paused", "notify-false"} {
		t.Run(scenario, func(t *testing.T) {
			n := &recordingNotifier{}
			m, _, _ := fixture(t, n)
			s := create(t, m)
			s, err := m.Save(t.Context(), s.ID, Patch{FailureThreshold: ptr(1)})
			if err != nil {
				t.Fatal(err)
			}
			at := m.now()
			if scenario == "pre-silence" {
				monitorCheck(t, m, s, false, at.Add(-time.Minute), nil)
			}
			monitorSilence(t, m, "all", nil, at, at.Add(time.Hour))
			switch scenario {
			case "paused":
				s, err = m.Save(t.Context(), s.ID, Patch{Enabled: ptr(false)})
			case "notify-false":
				s, err = m.Save(t.Context(), s.ID, Patch{Notify: ptr(false)})
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if scenario == "certificate" {
				expiry := at.Add(24 * time.Hour)
				monitorCheck(t, m, s, true, at, &expiry)
				if m.latest[s.ID].CertificateNotifiedDay != "" || len(m.events) != 0 {
					t.Fatal("silenced certificate consumed budget")
				}
				monitorCheck(t, m, s, true, at.Add(time.Hour), &expiry)
				monitorCheck(t, m, s, true, at.Add(time.Hour+time.Minute), &expiry)
				want = 1
			} else {
				monitorCheck(t, m, s, false, at, nil)
				recovery := at.Add(time.Minute)
				if scenario == "recovery-after-end" {
					recovery = at.Add(time.Hour)
				}
				monitorCheck(t, m, s, true, recovery, nil)
				if scenario == "pre-silence" {
					want = 2
				}
			}
			monitorCounts(t, m, n, want)
		})
	}
}

func TestMonitorSilenceCurrentGroupReleaseAndQueryFailure(t *testing.T) {
	for _, scenario := range []string{"delete-silence", "delete-group", "move", "query-error"} {
		t.Run(scenario, func(t *testing.T) {
			n := &recordingNotifier{}
			m, db, _ := fixture(t, n)
			g, err := m.SaveGroup(t.Context(), "", "monitors")
			if err != nil {
				t.Fatal(err)
			}
			s := create(t, m)
			raw, _ := json.Marshal(g.ID)
			s, err = m.Save(t.Context(), s.ID, Patch{GroupID: raw, FailureThreshold: ptr(1)})
			if err != nil {
				t.Fatal(err)
			}
			at := m.now()
			monitorSilence(t, m, "service_group", g.ID, at, at.Add(time.Hour))
			monitorCheck(t, m, s, false, at, nil)
			monitorCounts(t, m, n, 0)
			switch scenario {
			case "delete-silence":
				_, err = silenceExec(t, db, "DELETE FROM alert_silences")
			case "delete-group":
				if err = m.DeleteGroup(t.Context(), g.ID); err != nil {
					t.Fatal(err)
				}
				s, err = m.Get(t.Context(), s.ID)
			case "move":
				s, err = m.Save(t.Context(), s.ID, Patch{GroupID: json.RawMessage(`null`)})
			case "query-error":
				if _, err = silenceExec(t, db, "ALTER TABLE alert_silences RENAME TO unavailable_silences"); err != nil {
					t.Fatal(err)
				}
				if err = m.Accept(t.Context(), s, Result{CheckedAt: at}); err == nil {
					t.Fatal("query error ignored")
				}
				if len(m.latest) != 0 || len(m.events) != 0 {
					t.Fatal("failed query changed buffered state")
				}
				if _, err = silenceExec(t, db, "ALTER TABLE unavailable_silences RENAME TO alert_silences"); err != nil {
					t.Fatal(err)
				}
				_, err = silenceExec(t, db, "DELETE FROM alert_silences")
			}
			if err != nil {
				t.Fatal(err)
			}
			monitorCheck(t, m, s, false, at.Add(time.Minute), nil)
			monitorCounts(t, m, n, 1)
			monitorCheck(t, m, s, false, at.Add(2*time.Minute), nil)
			monitorCounts(t, m, n, 1)
		})
	}
}
