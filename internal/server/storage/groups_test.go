package storage

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestGroupsDisplayOrderSchema(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(mainSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO groups(id,name) VALUES('default','default')"); err != nil {
		t.Fatal(err)
	}
	var order int
	if err = db.QueryRow("SELECT display_order FROM groups WHERE id='default'").Scan(&order); err != nil || order != 0 {
		t.Fatalf("default order=%d err=%v", order, err)
	}
	for _, value := range []any{-1000000, 1000000, -1000001, 1000001, nil} {
		_, err = db.Exec("UPDATE groups SET display_order=? WHERE id='default'", value)
		valid := value == -1000000 || value == 1000000
		if (err == nil) != valid {
			t.Fatalf("order=%v valid=%t err=%v", value, valid, err)
		}
	}
	found := false
	for _, ddl := range mysqlMainSchema {
		if strings.HasPrefix(ddl, "CREATE TABLE `groups`") {
			found = true
			if !strings.Contains(ddl, "`display_order` BIGINT NOT NULL DEFAULT 0 CHECK(display_order BETWEEN -1000000 AND 1000000)") {
				t.Fatal("MySQL group order definition missing")
			}
		}
	}
	if !found {
		t.Fatal("MySQL groups definition missing")
	}
}

func TestGroupsReplicaRegistryCodec(t *testing.T) {
	var group TableSpec
	for _, table := range Tables(MainDomain) {
		if table.Name == "groups" {
			group = table
		}
	}
	want := []ColumnSpec{{"id", Text, false}, {"name", Text, false}, {"display_order", Integer, false}}
	if group.ID != 5 || !reflect.DeepEqual(columns(group), want) {
		t.Fatalf("group registry=%+v", group)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, order := range []int{-1000000, 0, 1000000} {
		query := "SELECT " + jsonImage(SQLite, group, "g", false) + " FROM (SELECT 'group' AS id,'name' AS name,? AS display_order) g"
		var raw string
		if err = db.QueryRow(query, order).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		values, err := decodeValues([]byte(raw), columns(group))
		if err != nil || !reflect.DeepEqual(values, []any{"group", "name", int64(order)}) {
			t.Fatalf("order=%d codec=%v err=%v", order, values, err)
		}
		if !strings.Contains(jsonImage(MySQL, group, "NEW", false), "NEW.`display_order`") {
			t.Fatal("MySQL replica image omits display_order")
		}
	}
	for _, raw := range []string{`[["t","67"],["t","6e"]]`, `[["t","67"],["t","6e"],["t","31"]]`} {
		if _, err = decodeValues([]byte(raw), columns(group)); !errors.Is(err, ErrReplicaDiverged) {
			t.Fatalf("invalid group image accepted: %s err=%v", raw, err)
		}
	}
}

func TestGroupsReplicaDisplayOrderApply(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	ctx := t.Context()
	for i, query := range []string{
		"INSERT INTO groups(id,name,display_order) VALUES('group','group',123)",
		"UPDATE groups SET display_order=-456 WHERE id='group'",
	} {
		replicaWrite(t, source, query)
		batch, err := readReplicaTransaction(ctx, source, int64(i))
		if err != nil {
			t.Fatal(err)
		}
		if err = applyReplicaTransaction(ctx, target, batch); err != nil {
			t.Fatal(err)
		}
		var order int
		want := 123
		if i == 1 {
			want = -456
		}
		if err = target.current.reader.QueryRowContext(ctx, "SELECT display_order FROM groups WHERE id='group'").Scan(&order); err != nil || order != want {
			t.Fatalf("replica order=%d want=%d err=%v", order, want, err)
		}
	}
}
