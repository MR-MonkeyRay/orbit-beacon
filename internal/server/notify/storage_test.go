package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func outageServiceFixture(t *testing.T) (*Service, *storage.Databases, Channel, *storagetest.EndpointProxy) {
	t.Helper()
	f := storagetest.Start(t)
	proxy, primary := storagetest.ProxyEndpoint(t, f.Endpoint("primary"))
	standby := f.Endpoint("standby")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	dbs, err := storage.OpenDatabases(t.Context(), config.DatabaseConfig{Primary: primary, Standby: &standby}, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbs.Close() })
	box, _ := secretbox.New(bytes.Repeat([]byte{7}, 32), "orbitbeacon notification channel v1")
	storageBox, _ := secretbox.New(bytes.Repeat([]byte{7}, 32), "orbitbeacon storage notifications v1")
	s := New(dbs.Main, box, Options{Storage: dbs, StorageBox: storageBox, Now: time.Now})
	name, kind, enabled := "outage", "webhook", true
	c, err := s.Save(t.Context(), "", Patch{Name: &name, Kind: &kind, Enabled: &enabled, Config: json.RawMessage(`{"url":"https://notify.example/private?secret=hidden"}`), Secret: json.RawMessage(`"signing-secret"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateStorageNotificationSettings(t.Context(), storage.StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{c.ID}}); err != nil {
		t.Fatal(err)
	}
	return s, dbs, c, proxy
}

func TestMySQLStorageSnapshotRevocationRejectsLatePublishAndLateGuard(t *testing.T) {
	s, dbs, c, _ := outageServiceFixture(t)
	ctx := t.Context()
	lease, err := dbs.Main.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.pauseStorageSnapshot(lease.Context(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = s.publishStorageSnapshot(lease.Context(), lease, old); err != nil {
		t.Fatal(err)
	}
	earlierRead, err := s.readStorageSnapshot(lease.Context(), lease)
	if err != nil || len(earlierRead.Channels) != 1 {
		t.Fatal("initial SQL snapshot missing", err)
	}
	for _, channel := range earlierRead.Channels {
		if bytes.Contains(channel.Ciphertext, []byte("hidden")) || bytes.Contains(channel.Ciphertext, []byte("signing-secret")) {
			t.Fatal("plaintext cache")
		}
		if _, _, err = s.openStorageChannel(channel); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Delete(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if published, err := dbs.PublishOutageSnapshot(ctx, old, earlierRead.Preferences, earlierRead.Channels); err != nil || published {
		t.Fatal("late read restored deleted destination", published, err)
	}
	// 旧请求先持租约、晚取真实 guard，必须重新校验已撤销的实体。
	if _, err = s.UpdateStorageNotificationSettings(lease.Context(), storage.StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{c.ID}}); err == nil {
		t.Fatal("late guard accepted deleted channel")
	}
	current, err := dbs.OutageSnapshot(ctx)
	if err != nil || !current.Ready || len(current.Channels) != 0 || len(current.Preferences.ChannelIDs) != 0 {
		t.Fatal("revocation lost", err)
	}
}

func TestMySQLStorageUnknownMutationRemainsPausedAndRepairCanRead(t *testing.T) {
	s, dbs, _, _ := outageServiceFixture(t)
	ctx := t.Context()
	lease, err := dbs.Main.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.pauseStorageSnapshot(lease.Context(), lease)
	if err != nil {
		t.Fatal(err)
	}
	s.finishStorageMutation(lease.Context(), lease, tx, token, storage.ErrCommitUnknown)
	snapshot, err := dbs.OutageSnapshot(ctx)
	if err != nil || snapshot.Ready {
		t.Fatal("unknown outcome reopened sending", err)
	}
	settings, err := s.StorageNotificationSettings(lease.Context())
	if err != nil || settings.SnapshotReady || len(settings.ChannelIDs) != 1 {
		t.Fatal("repair settings unavailable", err)
	}
	if err = s.SyncStorageSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	s.opts.StorageBox, _ = secretbox.New(bytes.Repeat([]byte{8}, 32), "orbitbeacon storage notifications v1")
	snapshot, err = dbs.OutageSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.openStorageChannel(snapshot.Channels[0]); !errors.Is(err, ErrSecretUnreadable) {
		t.Fatal("wrong purpose key read verified cache", err)
	}
	s.box, _ = secretbox.New(bytes.Repeat([]byte{8}, 32), "orbitbeacon notification channel v1")
	if err = s.SyncStorageSnapshot(ctx); err != nil {
		t.Fatal("bad channel blocked healthy main publication", err)
	}
	settings, err = s.StorageNotificationSettings(ctx)
	if err != nil || settings.SnapshotReady {
		t.Fatal("bad channel repair form blocked or snapshot reopened", err)
	}
}

func TestStoragePreferencesValidateAndDeleteReferences(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	settings, err := f.s.UpdateStorageNotificationSettings(ctx, storage.StorageNotificationPreferences{Inbox: false, ChannelIDs: []string{f.channel.ID}})
	if err != nil || settings.Inbox {
		t.Fatal(settings, err)
	}
	disabled := false
	if _, err = f.s.Save(ctx, f.channel.ID, Patch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.UpdateStorageNotificationSettings(ctx, storage.StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{f.channel.ID}}); err == nil {
		t.Fatal("disabled channel accepted")
	}
	if err = f.s.Delete(ctx, f.channel.ID); err != nil {
		t.Fatal(err)
	}
	settings, err = f.s.StorageNotificationSettings(ctx)
	if err != nil || len(settings.ChannelIDs) != 0 {
		t.Fatal("deleted preference reference retained", settings, err)
	}
}

func TestMySQLStorageSnapshotIsNotPausedBeforeActualWriteGuard(t *testing.T) {
	s, dbs, c, _ := outageServiceFixture(t)
	holder, err := dbs.Main.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	guard, err := holder.BeginWrite(holder.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Rollback()
	waiting, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = s.UpdateStorageNotificationSettings(waiting, storage.StorageNotificationPreferences{Inbox: false, ChannelIDs: []string{c.ID}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("request did not wait on actual write guard", err)
	}
	snapshot, err := dbs.OutageSnapshot(t.Context())
	if err != nil || !snapshot.Ready || !snapshot.Preferences.Inbox {
		t.Fatal("waiting request changed or paused verified snapshot", err)
	}
}

func TestMySQLStorageSnapshotReadFailureAfterGuardRemainsPaused(t *testing.T) {
	s, dbs, _, proxy := outageServiceFixture(t)
	lease, err := dbs.Main.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	guard, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.pauseStorageSnapshot(lease.Context(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = guard.Rollback(); err != nil {
		t.Fatal(err)
	}
	proxy.Pause()
	defer proxy.Resume()
	if err = s.publishStorageSnapshot(lease.Context(), lease, token); err == nil {
		t.Fatal("failed main unexpectedly refreshed snapshot")
	}
	snapshot, err := dbs.OutageSnapshot(t.Context())
	if err != nil || snapshot.Ready {
		t.Fatal("post-pause read failure resumed old cache", err)
	}
}

func TestMySQLStorageStartupFailureBeforeGuardPreservesVerifiedCache(t *testing.T) {
	s, dbs, _, proxy := outageServiceFixture(t)
	proxy.Pause()
	defer proxy.Resume()
	if err := s.SyncStorageSnapshot(t.Context()); err == nil {
		t.Fatal("unreachable main unexpectedly acquired write guard")
	}
	snapshot, err := dbs.OutageSnapshot(t.Context())
	if err != nil || !snapshot.Ready || len(snapshot.Channels) != 1 {
		t.Fatal("offline startup discarded verified cache", err)
	}
}

func TestMySQLAlertsAndStorageSharePersistentChannelRate(t *testing.T) {
	s, dbs, c, _ := outageServiceFixture(t)
	now := time.Now().UTC()
	s.opts.Now = func() time.Time { return now }
	for range 10 {
		allowed, _, configured, err := dbs.ReserveNotificationRate(t.Context(), c.ID, now)
		if err != nil || !configured || !allowed {
			t.Fatal("storage quota reservation failed", allowed, configured, err)
		}
		lease, err := dbs.Main.Acquire(t.Context(), storage.Writer)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := lease.BeginWrite(lease.Context())
		if err != nil {
			lease.Close()
			t.Fatal(err)
		}
		allowed, _, err = s.reserveRate(lease.Context(), tx, c.ID)
		tx.Rollback()
		lease.Close()
		if err != nil || !allowed {
			t.Fatal("alert quota reservation failed", allowed, err)
		}
	}
	allowed, next, configured, err := dbs.ReserveNotificationRate(t.Context(), c.ID, now)
	if err != nil || !configured || allowed || !next.Equal(now.Add(time.Minute)) {
		t.Fatal("combined twenty-first send allowed", allowed, next, configured, err)
	}
	lease, err := dbs.Main.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	allowed, _, err = s.reserveRate(lease.Context(), tx, c.ID)
	if err != nil || allowed {
		t.Fatal("alert bypassed shared exhausted quota", allowed, err)
	}
}
