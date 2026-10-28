package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"orbitbeacon/internal/timekey"
)

func TestSummaryCountsAndNewest(t *testing.T) {
	f := newFixture(t, 0)
	ctx := context.Background()
	empty, err := f.s.Summary(ctx)
	if err != nil || empty.Firing != (FiringCounts{}) || empty.Newest == nil || len(empty.Newest) != 0 {
		t.Fatalf("empty summary=%+v err=%v", empty, err)
	}
	// Tied timestamps exercise the ID tiebreaker. Legacy, null and
	// unrecognized severities must all count as unknown.
	snapshots := []string{
		`{"metric":"cpu_percent","severity":"info"}`,
		`{"metric":"cpu_percent","severity":"warning"}`,
		`{"metric":"cpu_percent","severity":"critical"}`,
		`{"severity":"warning"}`, `{}`, `{"severity":null}`, `{"severity":"future"}`,
	}
	for i, snapshot := range snapshots {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		f.exec(t, `INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,rule_name,node_name,rule_snapshot)
 VALUES(?,? ,?,'firing',90,?,'stored rule','stored node',?)`, id, f.rule.ID, id, timekey.Format(f.now.Add(time.Duration(i/2)*time.Second)), snapshot)
	}
	// The current rule's severity differs from some snapshots; deleted rules
	// and nodes must retain their stored display fields.
	f.exec(t, "UPDATE alert_events SET rule_id='deleted-rule' WHERE id=?", "00000000-0000-4000-8000-000000000007")
	f.exec(t, `INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,resolved_at,rule_name,node_name,rule_snapshot)
 VALUES('resolved',? ,?,'resolved',10,?,?,'resolved rule','resolved node','{"severity":"critical"}')`, f.rule.ID, f.node, timekey.Format(f.now.Add(time.Hour)), timekey.Format(f.now.Add(time.Hour)))
	summary, err := f.s.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantCounts := FiringCounts{Total: 7, Info: 1, Warning: 2, Critical: 1, Unknown: 3}
	if summary.Firing != wantCounts {
		t.Fatalf("counts=%+v want %+v", summary.Firing, wantCounts)
	}
	page, err := f.s.Events(ctx, "firing", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Newest) != 5 || !reflect.DeepEqual(summary.Newest, page.Events) {
		t.Fatalf("newest=%+v want %+v", summary.Newest, page.Events)
	}
	for i, event := range summary.Newest {
		wantID := fmt.Sprintf("00000000-0000-4000-8000-%012d", 7-i)
		if event.ID != wantID || event.State != "firing" {
			t.Fatalf("newest[%d]=%+v want %s", i, event, wantID)
		}
	}
	if summary.Newest[0].RuleName != "stored rule" || summary.Newest[0].NodeName != "stored node" {
		t.Fatalf("deleted metadata lost: %+v", summary.Newest[0])
	}
	f.exec(t, "DELETE FROM alert_events")
	cleared, err := f.s.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cleared)
	if err != nil || string(encoded) != `{"firing":{"total":0,"info":0,"warning":0,"critical":0,"unknown":0},"newest":[]}` {
		t.Fatalf("cleared JSON=%s err=%v", encoded, err)
	}
}

func TestSummaryDoesNotReserveWriter(t *testing.T) {
	f := newFixture(t, 0)
	f.tick(t)
	f.db.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	writer, err := f.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer writer.ExecContext(context.Background(), "ROLLBACK")
	if _, err := writer.ExecContext(ctx, "UPDATE alert_events SET state='resolved',resolved_at=?", timekey.Format(f.now)); err != nil {
		t.Fatal(err)
	}
	// A summary must read the committed firing event even while another
	// connection holds the writer and has an uncommitted resolution.
	summary, err := f.s.Summary(ctx)
	if err != nil || summary.Firing.Total != 1 || len(summary.Newest) != 1 {
		t.Fatalf("summary under writer contention=%+v err=%v", summary, err)
	}
}
