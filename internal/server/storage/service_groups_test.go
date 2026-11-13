package storage

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestServiceGroupsDisplayOrderSchema(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(mainSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO service_groups(id,name) VALUES('default','default')"); err != nil {
		t.Fatal(err)
	}
	var order int
	if err = db.QueryRow("SELECT display_order FROM service_groups WHERE id='default'").Scan(&order); err != nil || order != 0 {
		t.Fatalf("default order=%d err=%v", order, err)
	}
	for _, value := range []any{-1000000, 1000000, -1000001, 1000001, nil} {
		_, err = db.Exec("UPDATE service_groups SET display_order=? WHERE id='default'", value)
		valid := value == -1000000 || value == 1000000
		if (err == nil) != valid {
			t.Fatalf("order=%v valid=%t err=%v", value, valid, err)
		}
	}
	found := false
	mysqlGroupPosition, mysqlServicePosition := -1, -1
	for i, ddl := range mysqlMainSchema {
		if strings.HasPrefix(ddl, "CREATE TABLE `service_groups`") {
			found = true
			mysqlGroupPosition = i
			if !strings.Contains(ddl, "`display_order` BIGINT NOT NULL DEFAULT 0 CHECK(display_order BETWEEN -1000000 AND 1000000)") {
				t.Fatal("MySQL group order definition missing")
			}
		}
		if strings.HasPrefix(ddl, "CREATE TABLE services") {
			mysqlServicePosition = i
			if !strings.Contains(ddl, "group_id VARBINARY(64)") || !strings.Contains(ddl, "FOREIGN KEY (`group_id`) REFERENCES `service_groups`(`id`) ON DELETE SET NULL") {
				t.Fatal("MySQL service group FK missing")
			}
		}
	}
	if mysqlGroupPosition < 0 || mysqlServicePosition <= mysqlGroupPosition {
		t.Fatal("MySQL service group must precede services")
	}
	if !found {
		t.Fatal("MySQL groups definition missing")
	}
}

func TestServiceGroupsReplicaRegistryCodec(t *testing.T) {
	var group TableSpec
	for _, table := range Tables(MainDomain) {
		if table.Name == "service_groups" {
			group = table
		}
	}
	want := []ColumnSpec{{"id", Text, false}, {"name", Text, false}, {"display_order", Integer, false}}
	if group.ID != 36 || !reflect.DeepEqual(columns(group), want) {
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

func TestServiceGroupsReplicaDisplayOrderApply(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	ctx := t.Context()
	for i, query := range []string{
		"INSERT INTO service_groups(id,name,display_order) VALUES('group','group',123)",
		"UPDATE service_groups SET display_order=-456 WHERE id='group'",
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
		if err = target.current.reader.QueryRowContext(ctx, "SELECT display_order FROM service_groups WHERE id='group'").Scan(&order); err != nil || order != want {
			t.Fatalf("replica order=%d want=%d err=%v", order, want, err)
		}
	}
}

func TestServiceGroupsForeignKeyAndRegistryOrder(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON;" + mainSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO service_groups(id,name) VALUES('g','group'); INSERT INTO services(id,name,type,target,enabled,public,notify,group_id,created_at,updated_at) VALUES('s','service','tcp','localhost:443',1,0,0,'g','now','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE services SET group_id='missing' WHERE id='s'"); err == nil {
		t.Fatal("unknown group accepted")
	}
	if _, err = db.Exec("DELETE FROM service_groups WHERE id='g'"); err != nil {
		t.Fatal(err)
	}
	var group *string
	if err = db.QueryRow("SELECT group_id FROM services WHERE id='s'").Scan(&group); err != nil || group != nil {
		t.Fatalf("FK SET NULL=%v err=%v", group, err)
	}
	var hasIndex bool
	if err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='index' AND name='services_group')").Scan(&hasIndex); err != nil || !hasIndex {
		t.Fatal("group index missing", err)
	}
	for _, table := range Tables(MainDomain) {
		rows, err := db.Query("PRAGMA table_xinfo(`" + table.Name + "`)")
		if err != nil {
			t.Fatal(err)
		}
		actual := map[string]bool{}
		for rows.Next() {
			var cid, notnull, pk, hidden int
			var name, typ string
			var def any
			if err = rows.Scan(&cid, &name, &typ, &notnull, &def, &pk, &hidden); err != nil {
				t.Fatal(err)
			}
			actual[name] = hidden != 0
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			t.Fatal(err)
		}
		if len(actual) != len(table.Columns) {
			t.Fatalf("%s registry/schema column count mismatch", table.Name)
		}
		for _, c := range table.Columns {
			if generated, ok := actual[c.Name]; !ok || generated != c.Generated {
				t.Fatalf("%s.%s registry/schema mismatch", table.Name, c.Name)
			}
		}
	}
	seen := map[uint16]bool{}
	groupPos, servicePos := -1, -1
	for _, domain := range []Domain{MainDomain, HistoryDomain} {
		for i, table := range Tables(domain) {
			if seen[table.ID] {
				t.Fatalf("duplicate table ID %d", table.ID)
			}
			seen[table.ID] = true
			if table.Name == "service_groups" {
				groupPos = i
			}
			if table.Name == "services" {
				servicePos = i
				if table.ID != 33 || !reflect.DeepEqual(table.FK, []ForeignKeySpec{{"group_id", "service_groups", "id", "SET NULL"}}) {
					t.Fatalf("service registry=%+v", table)
				}
				found := false
				for _, c := range table.Columns {
					if c.Name == "group_id" {
						found = c.Type == Text && !c.Generated
					}
				}
				if !found {
					t.Fatal("missing service group replica column")
				}
			}
		}
	}
	if groupPos < 0 || groupPos >= servicePos {
		t.Fatalf("dependency order: group %d service %d", groupPos, servicePos)
	}
}

func TestServiceGroupsReplicaMembershipAndDeleteApply(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	ctx := t.Context()
	for i, query := range []string{
		"INSERT INTO service_groups(id,name,display_order) VALUES('g','group',7)",
		"INSERT INTO services(id,name,type,target,enabled,public,notify,group_id,created_at,updated_at) VALUES('s','service','tcp','localhost:443',1,0,0,'g','now','now')",
		"DELETE FROM service_groups WHERE id='g'",
	} {
		replicaWrite(t, source, query)
		batch, err := readReplicaTransaction(ctx, source, int64(i))
		if err != nil {
			t.Fatal(err)
		}
		if err = applyReplicaTransaction(ctx, target, batch); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			continue
		}
		var group *string
		if err = target.current.reader.QueryRowContext(ctx, "SELECT group_id FROM services WHERE id='s'").Scan(&group); err != nil {
			t.Fatal(err)
		}
		if i == 1 && (group == nil || *group != "g") || i == 2 && group != nil {
			t.Fatalf("replica membership=%v at step %d", group, i)
		}
	}
}
