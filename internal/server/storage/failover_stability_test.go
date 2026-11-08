package storage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestFailbackStabilityCrashChild(t *testing.T) {
	root := os.Getenv("ORBITBEACON_STABILITY_CRASH_ROOT")
	if root == "" {
		t.Skip("crash subprocess only")
	}
	r, d := crashDatabases(t, root, false)
	ctx := context.Background()
	source, err := OpenMain(ctx, filepath.Join(root, "primary", "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	d.source = source
	source.replica = d
	selected, err := position(ctx, d.target.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.switchTo(ctx, "standby", selected); err != nil {
		t.Fatal(err)
	}
	if err = d.synchronize(ctx); err != nil {
		t.Fatal(err)
	}
	if err = d.tryFailback(ctx); err != nil {
		t.Fatal(err)
	}
	if r.record.Domains[MainDomain].StableSince == nil {
		t.Fatal("writable stability was not persisted")
	}
	os.Exit(81)
}

func TestFailbackDoesNotReuseStabilityAfterRestart(t *testing.T) {
	root := t.TempDir()
	r, d := crashDatabases(t, root, true)
	d.source.Close()
	d.target.Close()
	r.control.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestFailbackStabilityCrashChild$")
	command.Env = append(os.Environ(), "ORBITBEACON_STABILITY_CRASH_ROOT="+root)
	output, err := command.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 81 {
		t.Fatalf("stability child did not reach its persisted boundary: %v %s", err, output)
	}
	r, d = crashDatabases(t, root, false)
	m := r.record.Domains[MainDomain]
	if m.Active != "standby" || m.StableSince == nil {
		t.Fatal("fixture did not persist standby recovery stability")
	}
	d.source = d.target
	d.source.current.id = m.Generation
	d.source.replica = d
	d.target, err = OpenMain(context.Background(), filepath.Join(root, "primary", "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.control.Close()
	defer d.source.Close()
	defer d.target.Close()
	now := *m.StableSince
	now.Nanos += int64(time.Minute)
	now.Wall = now.Wall.Add(time.Minute)
	r.clock = func() (string, int64, time.Time, error) { return now.Boot, now.Nanos, now.Wall, nil }
	if err = d.tryFailback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Active != "standby" {
		t.Fatal("a single post-restart probe reused old continuous stability")
	}
	start := now
	for second := 1; second < 60; second++ {
		now.Nanos = start.Nanos + int64(time.Duration(second)*time.Second)
		now.Wall = start.Wall.Add(time.Duration(second) * time.Second)
		if err = d.tryFailback(context.Background()); err != nil {
			t.Fatal(err)
		}
		if m.Active != "standby" {
			t.Fatalf("failed back before 60 continuous writable seconds: %d", second)
		}
	}
	now.Nanos = start.Nanos + int64(time.Minute)
	now.Wall = start.Wall.Add(time.Minute)
	if err = d.tryFailback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Active != "primary" || d.source.Generation() != 3 {
		t.Fatal("continuous full minute did not permit same-head failback", m.Active, d.source.Generation())
	}
}

func TestFailoverWaitsThirtySecondsWithVerifiedTenSecondWindow(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('bounded','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.freeze(ctx, "active_unreachable"); err != nil {
		t.Fatal(err)
	}
	frozen := *m.Freeze
	elapsed := 30*time.Second - time.Nanosecond
	d.control.clock = func() (string, int64, time.Time, error) {
		return frozen.Boot, frozen.Nanos + int64(elapsed), frozen.Wall.Add(elapsed), nil
	}
	if err := d.tryPromotion(ctx); err != nil {
		t.Fatal(err)
	}
	if m.Active != "primary" || m.Intent != nil || d.source.Ready() {
		t.Fatal("promoted before the default continuous failure timeout")
	}
	elapsed = 30 * time.Second
	if err := d.tryPromotion(ctx); err != nil {
		t.Fatal(err)
	}
	if m.Active != "standby" || m.LossNanos == nil || *m.LossNanos < 0 || *m.LossNanos > int64(10*time.Second) {
		t.Fatal("default proof did not bound the selected prefix", m.Active, m.LossNanos)
	}
}
