package storage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func replicaBareMain(t *testing.T) *Database {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := OpenMain(context.Background(), filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func TestReplicaProtocolPartialAndCorruptDefinitionsRefusedWithoutDDL(t *testing.T) {
	cases := []struct {
		name                                               string
		tables                                             int
		missingEpoch, missingState, badHead, badDefinition bool
	}{{name: "only_identity", tables: 1}, {name: "two_tables", tables: 2}, {name: "three_tables", tables: 3}, {name: "four_tables", tables: 4}, {name: "missing_epoch", tables: 5, missingEpoch: true}, {name: "missing_state", tables: 5, missingState: true}, {name: "missing_head", tables: 5, badHead: true}, {name: "wrong_definition", tables: 5, badDefinition: true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := replicaBareMain(t)
			if _, err := db.current.writer.ExecContext(ctx, "INSERT INTO settings VALUES('kept','original')"); err != nil {
				t.Fatal(err)
			}
			for i, q := range replicaDDL(SQLite)[:tc.tables] {
				if tc.badDefinition && i == 1 {
					q = strings.Replace(q, "head_seq INTEGER", "head_seq TEXT", 1)
				}
				if _, err := db.current.writer.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.current.writer.ExecContext(ctx, "INSERT INTO replica_identity VALUES(1,'resource','instance','main',?)", CurrentSchemaVersion); err != nil {
				t.Fatal(err)
			}
			if tc.tables >= 3 && !tc.missingEpoch {
				if _, err := db.current.writer.ExecContext(ctx, "INSERT INTO replica_epochs VALUES('epoch','',0,'')"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.tables >= 2 && !tc.missingState {
				head, id := int64(0), ""
				if tc.badHead {
					head, id = 1, uuid.NewString()
				}
				if _, err := db.current.writer.ExecContext(ctx, "INSERT INTO replica_state VALUES(1,'epoch',?,?,1,'off','',0,0,0,0)", head, id); err != nil {
					t.Fatal(err)
				}
			}
			before, err := replicaProtocolObjects(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if err = installReplica(ctx, db, "resource", "instance", "epoch", replicaProof{}); err == nil {
				t.Error("incomplete or corrupt protocol was adopted")
			}
			after, err := replicaProtocolObjects(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Error("rejected protocol changed schema objects")
			}
			var value string
			if err = db.current.reader.QueryRowContext(ctx, "SELECT value FROM settings WHERE `key`='kept'").Scan(&value); err != nil || value != "original" {
				t.Fatal("standalone data changed", value, err)
			}
		})
	}
}
