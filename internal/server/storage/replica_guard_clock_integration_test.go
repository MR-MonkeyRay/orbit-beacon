package storage_test

import (
	"testing"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLBarrierSamplesAfterBlockedNativeWriteGuard(t *testing.T) {
	fixture := storagetest.Start(t)
	cfg, dir := coreConfig(t, fixture)
	dbs, err := storage.OpenDatabases(t.Context(), cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	storage.ReplicaTestGuardBarrierSampling(t, dbs.Main, cfg.Primary)
}
