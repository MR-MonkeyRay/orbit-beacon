package backup

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
	"orbitbeacon/internal/timekey"
)

func TestMySQLBackupRunRecoversNewActiveWorkWithoutReupload(t *testing.T) {
	f := storagetest.Start(t)
	proxy, primary := storagetest.ProxyEndpoint(t, f.Endpoint("primary"))
	standby := f.Endpoint("standby")
	dir := framingTempDir(t)
	initial := restoreFixture(t, "worker")
	if err := copyRestoreFile(t.Context(), filepath.Join(initial, "secrets.key"), filepath.Join(dir, "secrets.key")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	for i, domain := range []storage.Domain{storage.MainDomain, storage.HistoryDomain} {
		db, err := storage.OpenDomain(ctx, primary, domain, dir)
		if err != nil {
			t.Fatal(err)
		}
		err = storage.ReplaceDomain(ctx, db, filepath.Join(initial, restoreInstallFiles[i]), uuid.NewString())
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.DatabaseConfig{Primary: primary, Standby: &standby, Failover: config.FailoverConfig{FailureTimeout: time.Second, MaxLossWindow: 10 * time.Second, RecoveryStable: time.Minute, ManualAfter: time.Hour}}
	dbs, err := storage.OpenDatabases(ctx, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	if err = dbs.CompleteBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := dbs.Main.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	_, err = tx.ExecContext(lease.Context(), "DELETE FROM backup_plans")
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	lease.Close()
	if err != nil {
		t.Fatal(err)
	}
	key, err := datadir.LoadKey(filepath.Join(dir, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key, "orbitbeacon backup secrets v1")
	clear(key)
	if err != nil {
		t.Fatal(err)
	}
	uploader := &recordingUploader{}
	s, err := New(dbs.Main, Options{DataDir: dir, History: dbs.History, SecretBox: box, Uploader: uploader, ServerVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = dbs.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	work, stop := context.WithCancel(ctx)
	go func() { defer close(done); s.Run(work) }()
	defer func() { stop(); <-done }()
	first, err := s.CreateExport(ctx, restoreUser, []Category{Configuration})
	if err != nil {
		t.Fatal(err)
	}
	source, target := f.RawEndpoint(t, "primary", storage.MainDomain), f.RawEndpoint(t, "standby", storage.MainDomain)
	deadline := time.Now().Add(30 * time.Second)
	for {
		var state string
		err = source.QueryRow("SELECT state FROM backup_tasks WHERE id=?", first.ID).Scan(&state)
		if err == nil && state == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("same worker did not finish initial timeline job", state, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	type memberState struct {
		Ready         bool
		State, Reason string
		Rebuild       *struct{ Phase string }
	}
	readMain := func() (memberState, error) {
		raw, err := dbs.CaptureRestoreMetadata(ctx)
		if err != nil {
			return memberState{}, err
		}
		var record struct {
			Domains map[storage.Domain]memberState
		}
		err = json.Unmarshal(raw, &record)
		return record.Domains[storage.MainDomain], err
	}
	deadline = time.Now().Add(30 * time.Second)
	for {
		member, readErr := readMain()
		if readErr == nil && member.Ready && member.Rebuild == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("initial target never completed registered seed: %+v err=%v", member, readErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	id := uuid.NewString()
	cipher, err := box.Seal("task-password:"+id, []byte("ABCDEFGHIJKLMNOP"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err = dbs.Main.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err = lease.BeginWrite(lease.Context())
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	_, err = tx.ExecContext(lease.Context(), "INSERT INTO backup_tasks(id,kind,state,phase,categories_json,destination_json,password_ciphertext,created_at) VALUES(?,'backup_export','running','storing','[\"configuration\"]','{}',?,?)", id, cipher, timekey.Format(time.Now()))
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	lease.Close()
	if err != nil {
		t.Fatal(err)
	}
	var epoch, head string
	var seq int64
	if err = source.QueryRow("SELECT epoch_uuid,head_seq,head_tx_uuid FROM replica_state WHERE id=1").Scan(&epoch, &seq, &head); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(30 * time.Second)
	for {
		var count int
		_ = target.QueryRow("SELECT count(*) FROM replica_transactions WHERE epoch_uuid=? AND seq=? AND tx_uuid=? AND boot_id<>'' AND barrier_ns>0", epoch, seq, head).Scan(&count)
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new running task never reached proven candidate")
		}
		time.Sleep(20 * time.Millisecond)
	}
	generation := dbs.Main.Generation()
	proxy.Pause()
	defer proxy.Resume()
	deadline = time.Now().Add(50 * time.Second)
	for {
		var state, code, warning string
		readErr := target.QueryRow("SELECT state,error_code,warning_code FROM backup_tasks WHERE id=?", id).Scan(&state, &code, &warning)
		if readErr == nil && state == "failed" && code == "interrupted" && warning == "storage_outcome_unknown" && dbs.Main.Generation() > generation && dbs.Main.Ready() {
			break
		}
		if time.Now().After(deadline) {
			member, metadataErr := readMain()
			t.Logf("main_control=%+v metadata_error=%v", member, metadataErr)
			t.Fatalf("same worker failed to recover candidate running job: generation=%d ready=%v state=%s code=%s warning=%s err=%v", dbs.Main.Generation(), dbs.Main.Ready(), state, code, warning, readErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if uploader.calls.Load() != 0 {
		t.Fatal("uncertain external upload was repeated")
	}
	select {
	case <-done:
		t.Fatal("backup worker exited instead of surviving generation change")
	default:
	}
}
