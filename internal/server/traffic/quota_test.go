package traffic_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"testing"
	"time"
)

func TestBillingModesAndQuotaBatch(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		mode    string
		used    string
		percent float64
	}{{"total", "300", 150}, {"max", "170", 85}, {"rx", "130", 65}, {"tx", "170", 85}} {
		insertNode(t, db, tc.mode, 1, "200")
		if _, err := db.Exec("UPDATE nodes SET billing_mode=? WHERE id=?", tc.mode, tc.mode); err != nil {
			t.Fatal(err)
		}
		insertHourly(t, db, tc.mode, "a", now, 100, 20, now)
		insertHourly(t, db, tc.mode, "b", now.Add(-time.Hour), 0, 110, now)
		if _, err := db.Exec("INSERT INTO traffic_daily(node_id,day,rx_bytes,tx_bytes) VALUES(?,?,30,40)", tc.mode, "2026-10-14"); err != nil {
			t.Fatal(err)
		}
		got, err := service.Read(context.Background(), tc.mode, 30, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.BillingMode != tc.mode || got.Cycle.UsedBytes != tc.used {
			t.Fatalf("%s: %+v", tc.mode, got)
		}
		values, err := service.Quota(context.Background(), now)
		if err != nil || values[tc.mode] != tc.percent {
			t.Fatalf("%s: values=%v err=%v", tc.mode, values, err)
		}
	}
	insertNode(t, db, "unlimited", 1, "0")
	insertNode(t, db, "empty", 1, "200")
	insertNode(t, db, "previous-month", 28, "100")
	insertNode(t, db, "current-month", 1, "100")
	for _, id := range []string{"previous-month", "current-month"} {
		insertHourly(t, db, id, "a", time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), 30, 20, now)
	}
	values, err := service.Quota(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values["unlimited"]; ok {
		t.Fatal("unlimited node included")
	}
	if v, ok := values["empty"]; !ok || v != 0 {
		t.Fatalf("empty quota node=%v", values)
	}
	if values["previous-month"] != 50 || values["current-month"] != 0 {
		t.Fatalf("billing windows=%v", values)
	}
	rollover, err := service.Quota(context.Background(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || rollover["total"] != 0 {
		t.Fatalf("rollover=%v err=%v", rollover, err)
	}
	// 保留任务迁移源行后，历史读取仍须得到相同总量。
	if _, err := service.Cleanup(context.Background(), now.AddDate(0, 0, 40), 100); err != nil {
		t.Fatal(err)
	}
	after, err := service.Quota(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	for id, value := range values {
		if after[id] != value {
			t.Fatalf("retention changed %s: %v -> %v", id, value, after[id])
		}
	}
}

func TestBillingModesSaturateAndAcceptUint64Quota(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, mode := range []string{"total", "max", "rx", "tx"} {
		insertNode(t, db, mode, 1, "18446744073709551615")
		if _, err := db.Exec("UPDATE nodes SET billing_mode=? WHERE id=?", mode, mode); err != nil {
			t.Fatal(err)
		}
		insertHourly(t, db, mode, "a", now, math.MaxInt64, math.MaxInt64, now)
		insertHourly(t, db, mode, "b", now.Add(-time.Hour), math.MaxInt64, math.MaxInt64, now)
		got, err := service.Read(context.Background(), mode, 1, now)
		if err != nil || got.Cycle.UsedBytes != "9223372036854775807" {
			t.Fatalf("%s: %+v %v", mode, got, err)
		}
	}
	values, err := service.Quota(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	for id, v := range values {
		if math.Abs(v-50) > 1e-10 {
			t.Fatalf("%s: percent=%v", id, v)
		}
	}
}

func TestQuotaBatchUsesOneQuery(t *testing.T) {
	// 结构约束：批量读取函数内只有一个 SQL 调用，且不在循环中。
	file, err := parser.ParseFile(token.NewFileSet(), "traffic.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	var walk func(ast.Node, bool)
	walk = func(n ast.Node, inLoop bool) {
		if n == nil {
			return
		}
		if _, ok := n.(*ast.ForStmt); ok {
			inLoop = true
		}
		if _, ok := n.(*ast.RangeStmt); ok {
			inLoop = true
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "QueryContext" || sel.Sel.Name == "QueryRowContext" || sel.Sel.Name == "BeginTx") {
				calls++
				if inLoop {
					t.Error("SQL call inside loop")
				}
			}
		}
		ast.Inspect(n, func(child ast.Node) bool {
			if child == n {
				return true
			}
			walk(child, inLoop)
			return false
		})
	}
	for _, decl := range file.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Name.Name == "Quota" {
			walk(f.Body, false)
		}
	}
	if calls != 1 {
		t.Fatalf("SQL calls=%d", calls)
	}
}

func TestQuotaBatchFor200Nodes(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for i := range 200 {
		insertNode(t, db, fmt.Sprint(i), 1, "100")
		insertHourly(t, db, fmt.Sprint(i), "a", now, 1, 2, now)
	}
	values, err := service.Quota(context.Background(), now)
	if err != nil || len(values) != 200 {
		t.Fatalf("batch size=%d err=%v", len(values), err)
	}
	for id, v := range values {
		if v != 3 {
			t.Fatalf("node %s: %v", id, v)
		}
	}
}
