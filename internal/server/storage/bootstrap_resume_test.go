package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"orbitbeacon/internal/config"
)

func TestInitialBootstrapResumesUnregisteredPrimaryWithoutRecreatingEstablishedMember(t *testing.T) {
	for _, pending := range []bool{true, false} {
		t.Run(map[bool]string{true: "initial_pending", false: "established"}[pending], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			r, err := openControl(ctx, dir, true)
			if err != nil {
				t.Fatal(err)
			}
			r.cfg = config.DatabaseConfig{Primary: config.DatabaseEndpoint{Driver: "sqlite"}, Standby: &config.DatabaseEndpoint{Driver: "sqlite"}}
			m := &memberRecord{Resource: uuid.NewString(), Primary: uuid.NewString(), Standby: uuid.NewString(), Epoch: uuid.NewString(), Active: "primary", State: "initializing", Generation: 1}
			r.record = controlRecord{Cluster: uuid.NewString(), Binding: configBinding(r.cfg), BootstrapPending: pending, Bootstrapped: !pending, Domains: map[Domain]*memberRecord{MainDomain: m}}
			if err = r.persistLocked(ctx); err != nil {
				t.Fatal(err)
			}
			dbs := &Databases{replication: r}
			defer dbs.Close()
			err = r.openRegistered(ctx, dbs, []Domain{MainDomain})
			if pending {
				if err != nil {
					t.Fatal("initial assigned identity could not finish registration", err)
				}
				if !m.PrimaryRegistered || !dbs.Main.Ready() || !dbs.BootstrapRequired() {
					t.Fatal("initial enrollment was not persisted before bootstrap")
				}
				instance, err := dbs.Main.InstanceID(ctx)
				if err != nil || instance != m.Primary {
					t.Fatal("resumption changed the assigned identity", instance, err)
				}
				lease, err := dbs.Main.Acquire(ctx, Reader)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
				var users int
				if err = lease.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&users); err != nil || users != 0 {
					t.Fatal("storage recreated an administrator", users, err)
				}
			} else {
				if err == nil && dbs.Main.Ready() {
					t.Fatal("established missing membership was recreated")
				}
				if _, err = os.Stat(filepath.Join(dir, "orbitbeacon.db")); !os.IsNotExist(err) {
					t.Fatal("unregistered established source was initialized", err)
				}
			}
		})
	}
}
