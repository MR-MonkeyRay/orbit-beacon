package notify

import (
	"context"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestRunStorageStandaloneReturnsWithoutWaitingForWriter(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "nil_storage"
		if explicit {
			name = "standalone_databases"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if explicit {
				f.s.opts.Storage = &storage.Databases{Main: f.s.db}
			}
			lease, err := f.s.db.Acquire(t.Context(), storage.Writer)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			guard, err := lease.BeginWrite(lease.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Rollback()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); f.s.RunStorage(ctx) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("standalone storage worker did not stop after cancellation")
				}
				t.Fatal("standalone storage worker waited for the occupied writer or started a background loop")
			}
		})
	}
}
