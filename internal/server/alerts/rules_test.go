package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
)

func TestRuleValidation(t *testing.T) {
	f := newFixture(t, 0)
	for _, raw := range []string{
		`{"name":""}`, `{"name":"   "}`, `{"name":"` + strings.Repeat("x", 101) + `"}`, `{"name":null}`, `{"enabled":null}`, `{"enabled":1}`, `{"threshold":null}`, `{"threshold":101}`, `{"threshold":-1}`, `{"threshold":"NaN"}`, `{"comparator":"="}`, `{"for_seconds":-1}`, `{"for_seconds":86401}`, `{"for_seconds":0.5}`, `{"severity":"bad"}`, `{"metric":"bad"}`, `{"scope_kind":"bad"}`, `{"scope_id":"00000000-0000-4000-8000-000000000099"}`, `{"scope_kind":"node","scope_id":null}`, `{"scope_kind":"node","scope_id":"00000000-0000-4000-8000-000000000099"}`, `{"metric":"offline","threshold":null,"comparator":null,"for_seconds":179}`, `{"metric":"offline","for_seconds":180}`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := f.s.Save(context.Background(), f.rule.ID, patch(t, raw))
			var v *validation.Error
			if !errors.As(err, &v) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	if _, err := f.s.Save(context.Background(), "", patch(t, `{}`)); err == nil {
		t.Fatal("missing create fields accepted")
	}
	if _, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"metric":"load1","threshold":1000000}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"metric":"load1","threshold":1000001}`)); err == nil {
		t.Fatal("unbounded load")
	}
	if _, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"metric":"probes_down","threshold":5}`)); err != nil {
		t.Fatal(err)
	}
	for _, threshold := range []string{"-1", "6"} {
		if _, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"metric":"probes_down","threshold":`+threshold+`}`)); err == nil {
			t.Fatalf("probes_down threshold %s accepted", threshold)
		}
	}
}

func TestProbesDownValidationBounds(t *testing.T) {
	comparator := ">"
	for _, threshold := range []float64{0, 5} {
		rule := Rule{Name: "probe failures", Enabled: true, ScopeKind: "all", Metric: "probes_down", Comparator: &comparator, Threshold: &threshold, Severity: "warning"}
		if err := validate(context.Background(), nil, rule); err != nil {
			t.Fatalf("threshold %v rejected: %v", threshold, err)
		}
	}
	for _, threshold := range []float64{-1, 6} {
		rule := Rule{Name: "probe failures", Enabled: true, ScopeKind: "all", Metric: "probes_down", Comparator: &comparator, Threshold: &threshold, Severity: "warning"}
		if err := validate(context.Background(), nil, rule); err == nil {
			t.Fatalf("threshold %v accepted", threshold)
		}
	}
}

func TestRuleAuditAtomicityAndCapacity(t *testing.T) {
	f := newFixture(t, 0)
	ctx := audit.WithActor(context.Background(), "admin")
	r, err := f.s.Save(ctx, "", patch(t, `{"name":"audited","enabled":true,"scope_kind":"all","metric":"cpu_percent","comparator":">","threshold":80,"for_seconds":0,"severity":"warning"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Save(ctx, r.ID, patch(t, `{"name":"updated"}`)); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Delete(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.db.QueryRow("SELECT count(*) FROM audit_events WHERE target_id=? AND action IN ('alert_rule.create','alert_rule.update','alert_rule.delete')", r.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("audit count=%d err=%v", count, err)
	}
	f.exec(t, `CREATE TRIGGER fail_alert_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'audit failure'); END`)
	if _, err = f.s.Save(ctx, f.rule.ID, patch(t, `{"name":"must roll back"}`)); err == nil {
		t.Fatal("failed audit committed")
	}
	after, _ := f.s.Get(ctx, f.rule.ID)
	if after.Name != f.rule.Name {
		t.Fatal("update not rolled back")
	}
	if err = f.s.Delete(ctx, f.rule.ID); err == nil {
		t.Fatal("delete with failed audit committed")
	}
	if _, err = f.s.Get(ctx, f.rule.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Save(ctx, "", patch(t, `{"name":"must roll back","enabled":true,"scope_kind":"all","metric":"cpu_percent","comparator":">","threshold":80,"for_seconds":0,"severity":"warning"}`)); err == nil {
		t.Fatal("create with failed audit committed")
	}
	f.exec(t, "DROP TRIGGER fail_alert_audit")
	for i := 1; i < MaxRules; i++ {
		f.exec(t, `INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES(?,'capacity',1,'all','cpu_percent','>',80,0,'info',?,?)`, fmt.Sprintf("00000000-0000-4000-8000-%012d", i), timekey.Format(f.now), timekey.Format(f.now))
	}
	if _, err = f.s.Save(ctx, "", patch(t, `{}`)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity err=%v", err)
	}
	if _, err = f.s.Save(ctx, f.rule.ID, patch(t, `{"name":"at capacity"}`)); err != nil {
		t.Fatal("update should work at capacity", err)
	}
}
func TestScopedRulesCascadeAndEventsSurvive(t *testing.T) {
	for _, scope := range []string{"node", "group"} {
		t.Run(scope, func(t *testing.T) {
			f := newFixture(t, 0)
			id := f.node
			if scope == "group" {
				id = "00000000-0000-4000-8000-000000000002"
				f.exec(t, "INSERT INTO groups(id,name) VALUES(?,'group')", id)
				f.exec(t, "UPDATE nodes SET group_id=?", id)
			}
			_, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"scope_kind":"`+scope+`","scope_id":"`+id+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			f.tick(t)
			table := "nodes"
			if scope == "group" {
				table = "groups"
			}
			f.exec(t, "DELETE FROM "+table+" WHERE id=?", id)
			if _, err := f.s.Get(context.Background(), f.rule.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("scope did not cascade", err)
			}
			f.tick(t)
			resolved := f.events(t, "resolved")
			if len(resolved) != 1 {
				t.Fatal("cascade erased history or failed to resolve")
			}
			// 展示元数据来自触发时的快照，而非已删除的规则。
			if resolved[0].Metric != f.rule.Metric || resolved[0].Severity != f.rule.Severity || resolved[0].Metric == "" {
				t.Fatalf("event lost rule snapshot metadata: %+v", resolved[0])
			}
		})
	}
}
func TestEventCursorAndRetentionBatch(t *testing.T) {
	f := newFixture(t, 0)
	for i := range 4 {
		f.advance(1)
		f.value(90)
		f.tick(t)
		f.advance(1)
		f.value(10)
		f.tick(t)
		if i == 3 {
			f.advance(1)
			f.value(90)
			f.tick(t)
		}
	}
	first, err := f.s.Events(context.Background(), "all", 2, "")
	if err != nil || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	f.advance(1)
	f.value(10)
	f.tick(t)
	f.advance(1)
	f.value(90)
	f.tick(t)
	next, err := f.s.Events(context.Background(), "all", 2, first.NextCursor)
	if err != nil || len(next.Events) != 2 {
		t.Fatal(err)
	}
	for _, a := range first.Events {
		for _, b := range next.Events {
			if a.ID == b.ID {
				t.Fatal("duplicate pagination")
			}
		}
	}
	for _, cursor := range []string{"bad", strings.Repeat("x", 90), first.NextCursor + "="} {
		if _, err := f.s.Events(context.Background(), "all", 2, cursor); err == nil {
			t.Fatal("bad cursor accepted")
		}
	}
	for _, state := range []string{"firing", "resolved"} {
		p, err := f.s.Events(context.Background(), state, 100, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range p.Events {
			if event.State != state {
				t.Fatal("state filter")
			}
		}
	}
	// 按恢复时间清理，保留近期恢复的记录和所有仍在触发中的记录。
	old := f.now.Add(-100 * 24 * time.Hour)
	f.exec(t, "UPDATE alert_events SET resolved_at=? WHERE state='resolved'", timekey.Format(old))
	resolved := f.events(t, "resolved")
	f.exec(t, "UPDATE alert_events SET resolved_at=? WHERE id=?", timekey.Format(f.now), resolved[0].ID)
	before := len(resolved)
	removed, err := f.s.Cleanup(context.Background(), f.now.Add(-90*24*time.Hour), 2)
	if err != nil || removed != 2 {
		t.Fatalf("cleanup=%d %v", removed, err)
	}
	if len(f.events(t, "resolved")) != before-2 || len(f.events(t, "firing")) != 1 {
		t.Fatal("cleanup was unbounded or removed firing")
	}
	if _, err = f.s.Cleanup(context.Background(), f.now, 0); err == nil {
		t.Fatal("zero limit")
	}
	// 保持 JSON 响应中的数组结构和 null 语义稳定。
	raw, err := json.Marshal(next)
	if err != nil || !strings.Contains(string(raw), `"events":[`) {
		t.Fatal(string(raw), err)
	}
}
