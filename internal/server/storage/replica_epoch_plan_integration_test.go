package storage_test

import (
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
	"testing"
)

func TestMySQLReplicaJournalUsesEpochPrimaryKey(t *testing.T) {
	f := storagetest.Start(t)
	endpoint := f.Endpoint("primary")
	storage.ReplicaTestEpochJournalPlan(t, f.Open(t, storage.MainDomain), &endpoint)
}
