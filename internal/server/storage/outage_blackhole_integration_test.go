package storage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLStorageNotificationCommitBlackholeKeepsSnapshotPaused(t *testing.T) {
	f := storagetest.Start(t)
	proxy, primary := newReplicaProxy(t, f.Endpoint("primary"))
	standby := f.Endpoint("standby")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dbs, err := storage.OpenDatabases(ctx, config.DatabaseConfig{Primary: primary, Standby: &standby}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	defer proxy.resume()
	if err = dbs.CompleteBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	channelBox, err := secretbox.New(bytes.Repeat([]byte{9}, 32), "orbitbeacon notification channel v1")
	if err != nil {
		t.Fatal(err)
	}
	storageBox, err := secretbox.New(bytes.Repeat([]byte{9}, 32), "orbitbeacon storage notifications v1")
	if err != nil {
		t.Fatal(err)
	}
	service := notify.New(dbs.Main, channelBox, notify.Options{Storage: dbs, StorageBox: storageBox, Now: time.Now})
	name, kind, enabled := "isolated storage notification", "webhook", true
	channel, err := service.Save(ctx, "", notify.Patch{Name: &name, Kind: &kind, Enabled: &enabled, Config: json.RawMessage(`{"url":"https://notify.example/storage"}`), Secret: json.RawMessage(`"isolated-signing-secret"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.UpdateStorageNotificationSettings(ctx, storage.StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{channel.ID}}); err != nil {
		t.Fatal(err)
	}
	before, err := dbs.OutageSnapshot(ctx)
	if err != nil || !before.Ready || len(before.Channels) != 1 {
		t.Fatal("initial channel snapshot was not verified", err)
	}
	raw := f.RawEndpoint(t, "primary", storage.MainDomain)
	var mutationsBefore int
	if err = raw.QueryRow("SELECT count(*) FROM replica_transactions WHERE change_count>0").Scan(&mutationsBefore); err != nil {
		t.Fatal(err)
	}
	generation := dbs.Main.Generation()
	var commitStarted time.Time
	observed, err := storage.ReplicaTestInstallCommitHook(dbs.Main, primary, false, func() { commitStarted = time.Now(); proxy.blackhole() })
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.UpdateStorageNotificationSettings(ctx, storage.StorageNotificationPreferences{Inbox: false, ChannelIDs: []string{}})
	elapsed := time.Since(commitStarted)
	if !errors.Is(err, storage.ErrCommitUnknown) {
		t.Fatalf("blackholed notification mutation did not return unknown: %v", err)
	}
	if commitStarted.IsZero() || elapsed < 4*time.Second || elapsed > 12*time.Second {
		t.Fatalf("COMMIT failed to leave real socket I/O: %s", elapsed)
	}
	select {
	case driverErr := <-observed:
		if driverErr == nil || !storage.IsConnectionFailure(driverErr) {
			t.Fatal("missing driver connection failure", driverErr)
		}
	case <-time.After(time.Second):
		t.Fatal("driver COMMIT observation missing")
	}
	if proxy.dropped.Load() == 0 {
		t.Fatal("complete COMMIT response was not blackholed")
	}
	snapshot, err := dbs.OutageSnapshot(ctx)
	if err != nil || snapshot.Ready {
		t.Fatal("unknown mutation republished an unverified destination", err)
	}
	var encoded string
	if err = raw.QueryRow("SELECT value FROM settings WHERE `key`='storage_notifications'").Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var persisted storage.StorageNotificationPreferences
	if err = json.Unmarshal([]byte(encoded), &persisted); err != nil || persisted.Inbox || len(persisted.ChannelIDs) != 0 {
		t.Fatal("native SQL did not contain the committed new preference", err)
	}
	var mutationsAfter int
	if err = raw.QueryRow("SELECT count(*) FROM replica_transactions WHERE change_count>0").Scan(&mutationsAfter); err != nil || mutationsAfter != mutationsBefore+1 {
		t.Fatal("native mutation was not committed exactly once", mutationsBefore, mutationsAfter, err)
	}
	proxy.resume()
	if err = dbs.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && (!dbs.Main.Ready() || dbs.Main.Generation() <= generation) {
		time.Sleep(10 * time.Millisecond)
	}
	if !dbs.Main.Ready() || dbs.Main.Generation() <= generation {
		t.Fatal("coordinator did not resolve and publish the recovered source")
	}
	snapshot, err = dbs.OutageSnapshot(ctx)
	if err != nil || snapshot.Ready {
		t.Fatal("generation recovery bypassed explicit snapshot calibration", err)
	}
	if err = service.SyncStorageSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err = dbs.OutageSnapshot(ctx)
	if err != nil || !snapshot.Ready || snapshot.Preferences.Inbox || len(snapshot.Channels) != 0 {
		t.Fatal("verified re-read did not publish the committed preferences", err)
	}
	if err = raw.QueryRow("SELECT count(*) FROM replica_transactions WHERE change_count>0").Scan(&mutationsAfter); err != nil || mutationsAfter != mutationsBefore+1 {
		t.Fatal("calibration automatically replayed the business mutation", mutationsBefore, mutationsAfter, err)
	}
}
