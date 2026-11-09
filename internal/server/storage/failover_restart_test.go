package storage

import (
	"context"
	"database/sql/driver"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"orbitbeacon/internal/config"
)

func crashDatabases(t *testing.T, root string, create bool) (*replication, *replicaDomain) {
	t.Helper()
	ctx := context.Background()
	if create {
		if err := os.Chmod(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"control", "primary", "standby"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	r, err := openControl(ctx, filepath.Join(root, "control"), create)
	if err != nil {
		t.Fatal(err)
	}
	r.cfg = config.DatabaseConfig{Primary: config.DatabaseEndpoint{Driver: "sqlite"}, Standby: &config.DatabaseEndpoint{Driver: "sqlite"}}
	var source *Database
	if create {
		source, err = OpenMain(ctx, filepath.Join(root, "primary", "orbitbeacon.db"))
		if err != nil {
			t.Fatal(err)
		}
		epoch := uuid.NewString()
		r.record = controlRecord{Cluster: uuid.NewString(), Binding: "[]", Domains: map[Domain]*memberRecord{MainDomain: {Resource: uuid.NewString(), Primary: uuid.NewString(), Standby: uuid.NewString(), Epoch: epoch, Active: "primary", State: "healthy", Ready: true, PrimaryRegistered: true, StandbyRegistered: true, Generation: 1}}}
	} else {
		source = unavailableDatabase(MainDomain)
	}
	target, err := OpenMain(ctx, filepath.Join(root, "standby", "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := r.record.Domains[MainDomain]
	if create {
		if err = installReplica(ctx, source, m.Resource, m.Primary, m.Epoch, replicaProof{}); err != nil {
			t.Fatal(err)
		}
		if err = installReplica(ctx, target, m.Resource, m.Standby, m.Epoch, replicaProof{}); err != nil {
			t.Fatal(err)
		}
	}
	d := &replicaDomain{source: source, target: target, control: r, resource: m.Resource, epoch: m.Epoch, domain: MainDomain}
	source.replica = d
	r.domains[MainDomain] = d
	if create {
		if _, err = source.current.writer.ExecContext(ctx, "UPDATE replica_state SET capture_mode='source' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		replicaWrite(t, source, "INSERT INTO settings VALUES('survives-crash','kept')")
		if err = d.catchUp(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err = target.current.writer.ExecContext(ctx, "UPDATE replica_state SET ready=1 WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if err = r.persistLocked(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return r, d
}

func TestFailoverSwitchCrashChild(t *testing.T) {
	root := os.Getenv("ORBITBEACON_SWITCH_CRASH_ROOT")
	if root == "" {
		t.Skip("crash subprocess only")
	}
	phase := os.Getenv("ORBITBEACON_SWITCH_CRASH_PHASE")
	r, d := crashDatabases(t, root, false)
	ctx := context.Background()
	// 正常切换的子进程先加载原权威；重启恢复则必须从不可准入句柄开始。
	source, err := OpenMain(ctx, filepath.Join(root, "primary", "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	d.source = source
	source.replica = d
	if phase == "rebuild" {
		if err = observeReplicaDatabase(ctx, d.target, nil, func(_ context.Context, q string, _ []driver.NamedValue) {
			if strings.HasPrefix(q, "INSERT INTO `settings`") {
				os.Exit(81)
			}
		}); err != nil {
			t.Fatal(err)
		}
		if err = d.seed(ctx); err != nil {
			t.Fatal(err)
		}
		t.Fatal("rebuild crash boundary was not reached")
		return
	}
	if phase == "intent" {
		if err = observeReplicaDatabase(ctx, d.target, nil, func(_ context.Context, q string, _ []driver.NamedValue) {
			if strings.HasPrefix(q, "INSERT INTO replica_epochs") {
				os.Exit(81)
			}
		}); err != nil {
			t.Fatal(err)
		}
	} else if phase == "marker" || phase == "admission" {
		handle := newDatabase(MainDomain, r.control, r.control)
		calls := 0
		if err = observeReplicaDatabase(ctx, handle, nil, func(_ context.Context, q string, _ []driver.NamedValue) {
			if strings.HasPrefix(q, "INSERT INTO storage_control") {
				calls++
				if (phase == "marker" && calls == 2) || (phase == "admission" && calls == 3) {
					os.Exit(81)
				}
			}
		}); err != nil {
			t.Fatal(err)
		}
		r.control = handle.current.writer
	} else if phase == "pointer" {
		source.hooks.Publish = func(context.Context, *Lease) error { os.Exit(81); return nil }
	} else {
		t.Fatal("unknown crash boundary")
	}
	selected, err := position(ctx, d.target.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.switchTo(ctx, "standby", selected); err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash boundary was not reached")
}

func TestFailoverPendingIntentSurvivesProcessCrash(t *testing.T) {
	for _, phase := range []string{"intent", "marker", "pointer", "admission"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			r, d := crashDatabases(t, root, true)
			if err := d.source.Close(); err != nil {
				t.Fatal(err)
			}
			if err := d.target.Close(); err != nil {
				t.Fatal(err)
			}
			if err := r.control.Close(); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestFailoverSwitchCrashChild$")
			command.Env = append(os.Environ(), "ORBITBEACON_SWITCH_CRASH_ROOT="+root, "ORBITBEACON_SWITCH_CRASH_PHASE="+phase)
			output, err := command.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 81 {
				t.Fatalf("child did not crash at %s: %v %s", phase, err, output)
			}
			r, d = crashDatabases(t, root, false)
			defer r.control.Close()
			defer d.source.Close()
			defer d.target.Close()
			m := r.record.Domains[MainDomain]
			if m.Intent == nil {
				t.Fatal("crash lost the pending decision")
			}
			epoch := m.Intent.Epoch
			published := 0
			d.source.hooks.Publish = func(ctx context.Context, l *Lease) error {
				if d.source.Ready() {
					t.Fatal("restarted candidate admitted before cache publication")
				}
				var value string
				if err := l.QueryRowContext(ctx, "SELECT value FROM settings WHERE `key`='survives-crash'").Scan(&value); err != nil {
					return err
				}
				if value != "kept" || l.Generation() != 2 {
					t.Fatal("recovery loaded a different timeline", value, l.Generation())
				}
				published++
				return nil
			}
			if err = d.finishSwitch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if m.Active != "standby" || m.Epoch != epoch || m.Generation != 2 || m.Intent != nil || published != 1 || !d.source.Ready() {
				t.Fatal("restart did not complete the same decision", m.Active, m.Epoch, m.Generation, published)
			}
			replicaWrite(t, d.source, "INSERT INTO settings VALUES('after-crash','new-authority')")
			primary, err := OpenReader(context.Background(), filepath.Join(root, "primary", "orbitbeacon.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer primary.Close()
			var count int
			if err = primary.QueryRow("SELECT count(*) FROM settings WHERE `key`='after-crash'").Scan(&count); err != nil || count != 0 {
				t.Fatal("restart wrote back to old primary", count, err)
			}
		})
	}
}

func TestFailoverRebuildRestartsAfterProcessCrash(t *testing.T) {
	root := t.TempDir()
	r, d := crashDatabases(t, root, true)
	d.source.Close()
	d.target.Close()
	r.control.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestFailoverSwitchCrashChild$")
	command.Env = append(os.Environ(), "ORBITBEACON_SWITCH_CRASH_ROOT="+root, "ORBITBEACON_SWITCH_CRASH_PHASE=rebuild")
	output, err := command.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 81 {
		t.Fatalf("child did not crash in copying: %v %s", err, output)
	}
	r, d = crashDatabases(t, root, false)
	source, err := OpenMain(context.Background(), filepath.Join(root, "primary", "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	d.source = source
	source.replica = d
	defer r.control.Close()
	defer d.source.Close()
	defer d.target.Close()
	m := r.record.Domains[MainDomain]
	if m.Ready || m.Rebuild == nil || m.Rebuild.Phase != "copying" {
		t.Fatal("crash lost the inactive rebuild operation")
	}
	if err = validateExistingReplica(context.Background(), d.target, m.Resource, m.Standby, replicaProof{Rebuilding: true}); err != nil {
		t.Fatal("registered incomplete target cannot restart its authorized rebuild", err)
	}
	if err = d.seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	var value string
	if err = d.target.current.reader.QueryRow("SELECT value FROM settings WHERE `key`='survives-crash'").Scan(&value); err != nil || value != "kept" {
		t.Fatal("restart used a half snapshot", value, err)
	}
	if !m.Ready || m.Rebuild != nil {
		t.Fatal("restarted snapshot was not completed")
	}
}
