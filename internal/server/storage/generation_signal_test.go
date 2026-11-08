package storage

import (
	"context"
	"testing"
)

func TestGenerationDoneTracksSelectedGenerationNotLease(t *testing.T) {
	unavailable := unavailableDatabase(MainDomain)
	if unavailable.GenerationDone() != nil {
		t.Fatal("unselected database invented a generation signal")
	}
	db := replicaBareMain(t)
	done := db.GenerationDone()
	if done == nil || done != db.GenerationDone() {
		t.Fatal("selected generation has no stable existing signal")
	}
	lease, err := db.Acquire(context.Background(), Reader)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	select {
	case <-done:
		t.Fatal("closing one lease canceled the selected generation")
	default:
	}
	replacement := replicaBareMain(t)
	if err = db.publishGeneration(context.Background(), replacement.current); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("retired generation did not wake its observer")
	}
	next := db.GenerationDone()
	if next == nil || next == done {
		t.Fatal("replacement generation reused its predecessor signal")
	}
	select {
	case <-next:
		t.Fatal("new generation was already canceled")
	default:
	}
}
