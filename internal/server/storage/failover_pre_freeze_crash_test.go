package storage

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCommitBeforeFreezeCrashChild(t *testing.T) {
	root := os.Getenv("ORBITBEACON_PRE_FREEZE_CRASH_ROOT")
	if root == "" {
		t.Skip("crash subprocess only")
	}
	r, d := crashDatabases(t, root, false)
	defer r.control.Close()
	source, err := OpenMain(t.Context(), filepath.Join(root, "primary", "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	d.source = source
	source.replica = d
	u := url.URL{Scheme: "file", Path: filepath.Join(root, "primary", "orbitbeacon.db")}
	u.RawQuery = url.Values{"_pragma": {"journal_mode(WAL)", "foreign_keys(ON)", "busy_timeout(5000)", "synchronous(FULL)", "mmap_size(0)", "cache_size(-8192)"}, "_txlock": {"immediate"}}.Encode()
	hook := &replicaHookConnector{Connector: &replicaSQLiteConnector{dsn: u.String(), driver: source.current.writer.Driver()}, before: func() {}, done: make(chan error, 1), afterCommit: func(err error) {
		if err == nil {
			os.Exit(83)
		}
	}}
	old := source.current.writer
	source.current.writer = sql.OpenDB(hook)
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := source.Acquire(t.Context(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), "INSERT INTO settings VALUES('committed-before-freeze','durable-tail')"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("child did not exit between native COMMIT and its storage receipt/F publication")
}

func TestCommitBeforeFreezeCrashUsesRestartTimeAndRejectsStaleCopy(t *testing.T) {
	root := t.TempDir()
	r, d := crashDatabases(t, root, true)
	var originalBoot string
	var originalBarrier int64
	if err := d.target.current.reader.QueryRowContext(t.Context(), "SELECT boot_id,barrier_ns FROM replica_transactions WHERE epoch_uuid=? AND seq=(SELECT head_seq FROM replica_state WHERE id=1)", d.epoch).Scan(&originalBoot, &originalBarrier); err != nil {
		t.Fatal(err)
	}
	d.source.Close()
	d.target.Close()
	r.control.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestCommitBeforeFreezeCrashChild$")
	command.Env = append(os.Environ(), "ORBITBEACON_PRE_FREEZE_CRASH_ROOT="+root)
	output, err := command.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 83 {
		t.Fatalf("child did not exit after real native commit: %v %s", err, output)
	}
	primary, err := OpenReader(t.Context(), filepath.Join(root, "primary", "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	var rows int
	if err = primary.QueryRowContext(t.Context(), "SELECT count(*) FROM settings WHERE `key`='committed-before-freeze' AND value='durable-tail'").Scan(&rows); err != nil || rows != 1 {
		t.Fatal("crash fixture did not commit a real source-only tail", rows, err)
	}
	time.Sleep(11 * time.Second)
	boot, restartedAt, err := replicaClock()
	if err != nil {
		t.Fatal(err)
	}
	if boot != originalBoot {
		t.Fatal("same-boot fixture crossed a host reboot")
	}
	r, d = crashDatabases(t, root, false)
	defer r.control.Close()
	defer d.source.Close()
	defer d.target.Close()
	m := r.record.Domains[MainDomain]
	if m.Freeze != nil || m.Intent != nil {
		t.Fatal("crash unexpectedly persisted F or an authority decision")
	}
	if d.source.possible.valid || d.source.pending != nil {
		t.Fatal("new process retained a possible-commit ledger/receipt")
	}
	// 尚无原主连接时，启动路径必须从当前真实时钟建立 F，而非复用进程内最后成功时间。
	_, unavailable := d.source.Acquire(t.Context(), Reader)
	if !errors.Is(unavailable, ErrUnavailable) {
		t.Fatal(unavailable)
	}
	if err = d.startupFailure(t.Context(), unavailable); err != nil {
		t.Fatal(err)
	}
	if m.Freeze == nil || m.Freeze.Boot != boot || m.Freeze.Nanos < restartedAt || m.Freeze.Nanos-originalBarrier <= int64(10*time.Second) {
		t.Fatal("restart invented an earlier freeze boundary", m.Freeze)
	}
	if m.Manual || !m.Ready {
		t.Fatal("fixture masked the RPO proof with another eligibility failure")
	}
	if err = d.target.current.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM settings WHERE `key`='committed-before-freeze'").Scan(&rows); err != nil || rows != 0 {
		t.Fatal("candidate unexpectedly contains the unreplicated commit", rows, err)
	}
	r.cfg.Failover.FailureTimeout = time.Second
	r.cfg.Failover.MaxLossWindow = 10 * time.Second
	time.Sleep(time.Second)
	if err = d.tryPromotion(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("stale candidate did not fail its actual RPO proof", err)
	}
	if m.Active != "primary" || m.Generation != 1 || m.Intent != nil || d.source.Ready() {
		t.Fatal("crash before F authorized an unproven promotion")
	}
}
