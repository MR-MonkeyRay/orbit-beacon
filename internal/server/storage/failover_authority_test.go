package storage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestFailoverOriginalIncidentDeadlineSurvivesStandbyFailure(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('incident','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.freeze(ctx, "active_unreachable"); err != nil {
		t.Fatal(err)
	}
	original := m.FailureSince
	first := *m.Freeze
	selected, err := position(ctx, d.target.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.switchTo(ctx, "standby", selected); err != nil {
		t.Fatal(err)
	}
	elapsed := 23 * time.Hour
	d.control.clock = func() (string, int64, time.Time, error) {
		return first.Boot, first.Nanos + int64(elapsed), first.Wall.Add(elapsed), nil
	}
	if err = d.freeze(ctx, "active_unreachable"); err != nil {
		t.Fatal(err)
	}
	if !m.FailureSince.Equal(original) {
		t.Error("standby failure extended the original incident deadline")
	}
	elapsed = 25 * time.Hour
	if err = d.checkManual(ctx); err != nil {
		t.Fatal(err)
	}
	if !m.Manual {
		t.Fatal("repeated failures renewed an expired automatic recovery authorization")
	}
}

func TestFailoverDurableIntentFinishesBeforeNewDeadlineLock(t *testing.T) {
	root := t.TempDir()
	r, d := crashDatabases(t, root, true)
	ctx := context.Background()
	if err := d.freeze(ctx, "active_unreachable"); err != nil {
		t.Fatal(err)
	}
	first := *r.record.Domains[MainDomain].Freeze
	d.source.Close()
	d.target.Close()
	r.control.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestFailoverSwitchCrashChild$")
	command.Env = append(os.Environ(), "ORBITBEACON_SWITCH_CRASH_ROOT="+root, "ORBITBEACON_SWITCH_CRASH_PHASE=pointer")
	output, err := command.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 81 {
		t.Fatalf("intent child did not reach its persisted boundary: %v %s", err, output)
	}
	r, d = crashDatabases(t, root, false)
	defer r.control.Close()
	defer d.source.Close()
	defer d.target.Close()
	m := r.record.Domains[MainDomain]
	if m.Intent == nil || m.Manual {
		t.Fatal("fixture did not persist a pre-deadline authorized intent")
	}
	selected := m.Intent.Epoch
	r.clock = func() (string, int64, time.Time, error) {
		return first.Boot, first.Nanos + int64(25*time.Hour), first.Wall.Add(25 * time.Hour), nil
	}
	if err = d.coordinatorStep(ctx); err != nil {
		t.Fatal("restart invalidated an already durable authority decision", err)
	}
	if m.Intent != nil || m.Epoch != selected || m.Active != "standby" || !d.source.Ready() {
		t.Fatal("restart abandoned the selected authority")
	}
	if err = d.checkManual(ctx); err != nil {
		t.Fatal(err)
	}
	if !m.Manual || !d.source.Ready() {
		t.Fatal("deadline must block later recovery, not stop the healthy selected authority")
	}
}
