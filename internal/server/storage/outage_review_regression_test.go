package storage_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/storage"
)

func outageReviewPublish(t *testing.T, databases *storage.Databases, generation uint64, preferences storage.StorageNotificationPreferences) storage.OutageSnapshotToken {
	t.Helper()
	ctx := context.Background()
	token, err := databases.PauseOutageSnapshot(ctx, generation)
	if err != nil {
		t.Fatal(err)
	}
	channels := make([]storage.OutageChannel, 0, len(preferences.ChannelIDs))
	for _, id := range preferences.ChannelIDs {
		channels = append(channels, storage.OutageChannel{ID: id, Ciphertext: []byte("encrypted channel fixture")})
	}
	if published, err := databases.PublishOutageSnapshot(ctx, token, preferences, channels); err != nil || !published {
		t.Fatal("snapshot not published", published, err)
	}
	return token
}

func outageReviewInsert(t *testing.T, databases *storage.Databases, event storage.OutageEvent, now time.Time) {
	t.Helper()
	ctx := context.Background()
	lease, err := databases.Main.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var role *string
	if event.ActiveRole != "" {
		role = &event.ActiveRole
	}
	details, err := json.Marshal(struct {
		Domain     storage.Domain `json:"domain"`
		Transition string         `json:"transition"`
		ActiveRole *string        `json:"active_role"`
		ReasonCode string         `json:"reason_code"`
		OccurredAt time.Time      `json:"occurred_at"`
	}{event.Domain, event.Transition, role, event.ReasonCode, event.OccurredAt})
	if err != nil {
		t.Fatal(err)
	}
	if err = inbox.Insert(lease.Context(), tx, inbox.Event{Kind: "storage", SourceID: event.ID, Transition: event.Transition, Severity: "critical", CreatedAt: now, Details: details}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = databases.MarkOutageProjected(ctx, event.ID, lease.Generation()); err != nil {
		t.Fatal(err)
	}
}

func outageReviewCount(t *testing.T, databases *storage.Databases, id string) int {
	t.Helper()
	lease, err := databases.Main.Acquire(context.Background(), storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var count int
	if err = lease.QueryRowContext(lease.Context(), "SELECT count(*) FROM inbox_notifications WHERE kind='storage' AND source_id=?", id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestOutageReviewSwitchCalibrationFansOutNewAuthorityPreferences(t *testing.T) {
	fixture := storage.ReplicaTestOutageFixture(t)
	databases := fixture.Databases
	ctx := context.Background()
	oldPreferences := storage.StorageNotificationPreferences{Inbox: false, ChannelIDs: []string{}}
	oldToken := outageReviewPublish(t, databases, 1, oldPreferences)
	if err := fixture.Promote(); err != nil {
		t.Fatal(err)
	}
	generation := databases.Main.Generation()
	if generation <= 1 {
		t.Fatal("real generation was not promoted")
	}
	if published, err := databases.PublishOutageSnapshot(ctx, oldToken, oldPreferences, nil); err != nil || published {
		t.Fatal("old generation snapshot was accepted", published, err)
	}
	if events, err := databases.OutageProjection(ctx, generation, 10); err != nil || len(events) != 0 {
		t.Fatal("uncalibrated event projected old preferences", events, err)
	}
	token, err := databases.PauseOutageSnapshot(ctx, generation)
	if err != nil {
		t.Fatal(err)
	}
	preferences := storage.StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{"restored-channel"}}
	if published, err := databases.PublishOutagePreferences(ctx, token, preferences); err != nil || !published {
		t.Fatal("new authority preferences were not published", published, err)
	}
	events, err := databases.OutageProjection(ctx, generation, 10)
	if err != nil {
		t.Fatal(err)
	}
	var activation *storage.OutageEvent
	for _, event := range events {
		if event.Transition == "standby_activated" {
			copy := event
			activation = &copy
		}
	}
	if activation == nil {
		t.Fatal("activation permanently inherited the discarded branch inbox=false")
	}
	outageReviewInsert(t, databases, *activation, fixture.Now())
	if deliveries, err := databases.ClaimOutageDeliveries(ctx, fixture.Now(), 4); err != nil || len(deliveries) != 0 {
		t.Fatal("external sends resumed before ciphertext calibration", deliveries, err)
	}
	channels := []storage.OutageChannel{{ID: "restored-channel", Ciphertext: []byte("verified new authority channel")}}
	if published, err := databases.PublishOutageSnapshot(ctx, token, preferences, channels); err != nil || !published {
		t.Fatal(published, err)
	}
	deliveries, err := databases.ClaimOutageDeliveries(ctx, fixture.Now(), 4)
	if err != nil {
		t.Fatal(err)
	}
	activationDeliveries := 0
	for _, delivery := range deliveries {
		if delivery.Event.ID == activation.ID {
			activationDeliveries++
		}
		if err = databases.FinishOutageDelivery(ctx, delivery.ID, storage.OutageResult{OK: true}, fixture.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if activationDeliveries != 1 || outageReviewCount(t, databases, activation.ID) != 1 {
		t.Fatal("new authority event did not reach both consumers once", activationDeliveries)
	}
	if published, err := databases.PublishOutageSnapshot(ctx, token, preferences, channels); err != nil || !published {
		t.Fatal(published, err)
	}
	if deliveries, err = databases.ClaimOutageDeliveries(ctx, fixture.Now(), 4); err != nil || len(deliveries) != 0 {
		t.Fatal("calibration replay duplicated a completed send", deliveries, err)
	}
}

func TestOutageReviewLongIncidentProjectsBeforeAndAfterRecovery(t *testing.T) {
	for _, recoverFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "recovered_before_projection"}[recoverFirst], func(t *testing.T) {
			fixture := storage.ReplicaTestOutageFixture(t)
			databases := fixture.Databases
			ctx := context.Background()
			outageReviewPublish(t, databases, 1, storage.StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{}})
			fixture.Manual(true)
			initial, err := databases.OutageProjection(ctx, 1, 10)
			if err != nil || len(initial) != 1 {
				t.Fatal(initial, err)
			}
			id := initial[0].ID
			fixture.Advance(31 * 24 * time.Hour)
			if recoverFirst {
				fixture.Manual(false)
			}
			events, err := databases.OutageProjection(ctx, 1, 10)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events {
				if event.ID == id {
					found = true
					outageReviewInsert(t, databases, event, fixture.Now())
					outageReviewInsert(t, databases, event, fixture.Now())
				}
			}
			if !found || outageReviewCount(t, databases, id) != 1 {
				t.Fatal("long incident vanished before real inbox insertion", found)
			}
			if !recoverFirst {
				fixture.Manual(false)
			}
			events, err = databases.OutageProjection(ctx, 1, 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.ID == id {
					t.Fatal("confirmed old event was not released to normal retention")
				}
			}
		})
	}
}
