package history

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDatabaseBuildsTrustedHistoryWithoutWorker(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "metrics.db")
	db, err := OpenDatabase(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO buckets(node_id,metric,step,t,total,peak,samples) VALUES('retained-node','cpu_percent',60,0,3,2,2)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO buckets(node_id,metric,step,t,total,peak,samples) VALUES('retained-node','cpu_percent',60,60,3,2,0)`); err == nil {
		t.Fatal("invalid bucket accepted")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDatabase(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var total float64
	if err = db.QueryRow(`SELECT total FROM buckets WHERE node_id='retained-node'`).Scan(&total); err != nil || total != 3 {
		t.Fatalf("retained history total=%v err=%v", total, err)
	}
}
