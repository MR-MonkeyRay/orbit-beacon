package storage_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLReplicaCompleteCommitResponseBlackholeResolvesGuard(t *testing.T) {
	f := storagetest.Start(t)
	proxy, primary := newReplicaProxy(t, f.Endpoint("primary"))
	standby := f.Endpoint("standby")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.DatabaseConfig{Primary: primary, Standby: &standby}
	ctx := context.Background()
	dbs, err := storage.OpenDatabases(ctx, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	generation := dbs.Main.Generation()
	if err = dbs.CompleteBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	observed, err := storage.ReplicaTestInstallCommitHook(dbs.Main, primary, false, proxy.blackhole)
	if err != nil {
		t.Fatal(err)
	}
	l, err := dbs.Main.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	tx, err := l.BeginWrite(l.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(l.Context(), "INSERT INTO settings VALUES('unknown-commit','once')"); err != nil {
		t.Fatal(err)
	}
	id := tx.ID()
	started := time.Now()
	err = tx.Commit()
	elapsed := time.Since(started)
	if !errors.Is(err, storage.ErrCommitUnknown) {
		t.Fatalf("blackholed COMMIT=%v", err)
	}
	l.Close()
	if elapsed < 4*time.Second || elapsed > 12*time.Second {
		t.Fatalf("COMMIT did not exit real socket I/O timeout: %s", elapsed)
	}
	if driverErr := <-observed; driverErr == nil || !storage.IsConnectionFailure(driverErr) {
		t.Fatalf("COMMIT driver outcome: %v", driverErr)
	}
	if proxy.dropped.Load() == 0 {
		t.Fatal("proxy did not consume the complete COMMIT response")
	}
	if dbs.Main.Ready() {
		t.Fatal("unresolved source reported ready")
	}
	proxy.resume()
	resolve, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	committed, err := tx.ResolveCommit(resolve)
	if err != nil || !committed {
		t.Fatalf("guard result committed=%v err=%v", committed, err)
	}
	if err = dbs.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (!dbs.Main.Ready() || dbs.Main.Generation() <= generation) {
		time.Sleep(10 * time.Millisecond)
	}
	if !dbs.Main.Ready() || dbs.Main.Generation() <= generation {
		t.Fatal("coordinator did not publish a recovered generation", dbs.Main.Generation())
	}
	raw := f.RawEndpoint(t, "primary", storage.MainDomain)
	var rows, logs int
	if err = raw.QueryRow("SELECT (SELECT count(*) FROM settings WHERE `key`='unknown-commit'),(SELECT count(*) FROM replica_transactions WHERE tx_uuid=?)", id).Scan(&rows, &logs); err != nil || rows != 1 || logs != 1 {
		t.Fatalf("commit was lost/replayed rows=%d logs=%d err=%v", rows, logs, err)
	}
	haWrite(t, dbs.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "UPDATE settings SET value='resolved' WHERE `key`='unknown-commit'"); err != nil {
			t.Fatal(err)
		}
	})
	committed, err = tx.ResolveCommit(resolve)
	if err != nil || !committed {
		t.Fatalf("later writer replaced original receipt: %v %v", committed, err)
	}
}

func TestMySQLReplicaUnknownTargetACKDoesNotReapplyTransaction(t *testing.T) {
	f := storagetest.Start(t)
	proxy, standby := newReplicaProxy(t, f.Endpoint("standby"))
	cfg := config.DatabaseConfig{Primary: f.Endpoint("primary"), Standby: &standby}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dbs, err := storage.OpenDatabases(ctx, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	target := storage.ReplicaTestStandby(dbs, storage.HistoryDomain)
	var resume *time.Timer
	observed, err := storage.ReplicaTestInstallCommitHook(target, standby, true, func() { proxy.blackhole(); resume = time.AfterFunc(6*time.Second, proxy.resume) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if resume != nil {
			resume.Stop()
		}
	})
	if err = dbs.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	sourceRaw := f.RawEndpoint(t, "primary", storage.HistoryDomain)
	targetRaw := f.RawEndpoint(t, "standby", storage.HistoryDomain)
	waitReplica(t, sourceRaw, targetRaw)
	l, err := dbs.History.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := l.BeginWrite(l.Context())
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(l.Context(), "INSERT INTO buckets VALUES('n','cpu',60,1,7,7,1)"); err != nil {
		tx.Rollback()
		l.Close()
		t.Fatal(err)
	}
	id := tx.ID()
	if err = tx.Commit(); err != nil {
		l.Close()
		t.Fatal(err)
	}
	l.Close()
	select {
	case driverErr := <-observed:
		if driverErr == nil || !storage.IsConnectionFailure(driverErr) {
			t.Fatalf("ACK was not unknown: %v", driverErr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("target COMMIT did not leave blackholed I/O")
	}
	if proxy.dropped.Load() == 0 {
		t.Fatal("target COMMIT response was not blackholed")
	}
	var total float64
	var samples int64
	if err = targetRaw.QueryRow("SELECT total,samples FROM buckets WHERE node_id='n' AND metric='cpu' AND step=60 AND t=1").Scan(&total, &samples); err != nil || total != 7 || samples != 1 {
		t.Fatalf("first committed batch total=%v samples=%d err=%v", total, samples, err)
	}
	// 新的源事务必须等目标解析前一个 ACK，不能把旧事务再累加一次。
	haWrite(t, dbs.History, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "UPDATE buckets SET total=total+3,samples=samples+1 WHERE node_id='n' AND metric='cpu' AND step=60 AND t=1"); err != nil {
			t.Fatal(err)
		}
	})
	waitReplica(t, sourceRaw, targetRaw)
	if err = targetRaw.QueryRow("SELECT total,samples FROM buckets WHERE node_id='n' AND metric='cpu' AND step=60 AND t=1").Scan(&total, &samples); err != nil || total != 10 || samples != 2 {
		t.Fatalf("ACK retry double-counted total=%v samples=%d err=%v", total, samples, err)
	}
	var logs int
	if err = targetRaw.QueryRow("SELECT count(*) FROM replica_transactions WHERE tx_uuid=?", id).Scan(&logs); err != nil || logs != 1 {
		t.Fatalf("duplicate target transaction=%d err=%v", logs, err)
	}
}
