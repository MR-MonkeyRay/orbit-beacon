package storage

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

func TestServiceFailureThresholdSchemaAndReplicaCodec(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(mainSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO services(id,name,type,target,enabled,public,notify,created_at,updated_at) VALUES('s','health','tcp','localhost:443',1,0,0,'now','now')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRow("SELECT failure_threshold FROM services WHERE id='s'").Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	var spec TableSpec
	for _, s := range Tables(MainDomain) {
		if s.Name == "services" {
			spec = s
		}
	}
	found := false
	for _, c := range columns(spec) {
		if c.Name == "failure_threshold" {
			found = true
			if c.Type != Integer {
				t.Fatal(c)
			}
		}
	}
	if !found {
		t.Fatal("replica registry lacks failure_threshold")
	}
	for _, value := range []any{1, 2, 5, 10, 0, 11, nil} {
		_, err = db.Exec("UPDATE services SET failure_threshold=? WHERE id='s'", value)
		valid := value == 1 || value == 2 || value == 5 || value == 10
		if (err == nil) != valid {
			t.Fatalf("value=%v err=%v", value, err)
		}
		if !valid {
			continue
		}
		var raw string
		if err = db.QueryRow("SELECT " + jsonImage(SQLite, spec, "s", false) + " FROM services s").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		values, err := decodeValues([]byte(raw), columns(spec))
		if err != nil {
			t.Fatal(err)
		}
		for i, c := range columns(spec) {
			if c.Name == "failure_threshold" && !reflect.DeepEqual(values[i], int64(value.(int))) {
				t.Fatal(values)
			}
		}
	}
	if !strings.Contains(jsonImage(MySQL, spec, "NEW", false), "NEW.`failure_threshold`") {
		t.Fatal("MySQL image missing threshold")
	}
	found = false
	for _, ddl := range mysqlMainSchema {
		if strings.HasPrefix(ddl, "CREATE TABLE services") {
			found = true
			if !strings.Contains(ddl, "failure_threshold BIGINT NOT NULL DEFAULT 2 CHECK(failure_threshold BETWEEN 1 AND 10)") {
				t.Fatal(ddl)
			}
		}
	}
	if !found {
		t.Fatal("MySQL services table missing")
	}
}

func TestServiceFailureThresholdReplicaApply(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	ctx := t.Context()
	for i, q := range []string{
		"INSERT INTO services(id,name,type,target,enabled,public,notify,failure_threshold,created_at,updated_at) VALUES('s','health','tcp','localhost:443',1,0,0,5,'now','now')",
		"UPDATE services SET failure_threshold=10 WHERE id='s'",
	} {
		replicaWrite(t, source, q)
		batch, err := readReplicaTransaction(ctx, source, int64(i))
		if err != nil {
			t.Fatal(err)
		}
		if err = applyReplicaTransaction(ctx, target, batch); err != nil {
			t.Fatal(err)
		}
		var n int
		want := 5
		if i == 1 {
			want = 10
		}
		if err = target.current.reader.QueryRowContext(ctx, "SELECT failure_threshold FROM services WHERE id='s'").Scan(&n); err != nil || n != want {
			t.Fatal(n, err)
		}
	}
}
