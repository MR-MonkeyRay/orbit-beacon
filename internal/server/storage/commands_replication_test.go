package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateInfoSurvivesReplicationAndSnapshot(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	replicaWrite(t, source,
		`INSERT INTO nodes(id,name,created_at) VALUES('node','node','original')`,
		`INSERT INTO agent_commands(id,node_id,credential_epoch,kind,update_info,state,result_code,requested_by,created_at,expires_at) VALUES('check','node',0,'check_update','{"channel":"github","current_version":"1.2.3","latest_version":"1.2.4","checked_at":"2030-01-01T00:00:00Z"}','succeeded','ok','operator','original','later')`,
	)
	batch, err := readReplicaTransaction(t.Context(), source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyReplicaTransaction(t.Context(), target, batch); err != nil {
		t.Fatal(err)
	}
	var replicated string
	if err := target.current.reader.QueryRow(`SELECT update_info FROM agent_commands WHERE id='check'`).Scan(&replicated); err != nil {
		t.Fatal(err)
	}
	lease, err := target.Acquire(t.Context(), Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "snapshot.db")
	if err := lease.Snapshot(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	db, err := Open(t.Context(), image)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var snapshot string
	if err := db.QueryRow(`SELECT update_info FROM agent_commands WHERE id='check'`).Scan(&snapshot); err != nil || snapshot != replicated || snapshot == "null" {
		t.Fatal(snapshot, replicated, err)
	}
}
