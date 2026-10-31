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
	// 相同时间戳用于检验 ID 决胜逻辑。遗留的、null 的和
	// 未识别的严重级别都必须计为 unknown。
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
	// 当前规则的严重级别与某些快照不同；已删除的规则和
	// 节点必须保留其已存储的展示字段。
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
	// 即使另一个连接持有写锁且有未提交的解决操作，
	// summary 也必须读到已提交的触发事件。
	summary, err := f.s.Summary(ctx)
	if err != nil || summary.Firing.Total != 1 || len(summary.Newest) != 1 {
		t.Fatalf("summary under writer contention=%+v err=%v", summary, err)
	}
}
