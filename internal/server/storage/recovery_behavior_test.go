package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func installOutageChannel(t *testing.T, dbs *Databases) {
	t.Helper()
	ctx := context.Background()
	token, err := dbs.PauseOutageSnapshot(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := dbs.PublishOutageSnapshot(ctx, token, StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{"channel"}}, []OutageChannel{{ID: "channel", Ciphertext: []byte("encrypted")}}); err != nil || !ok {
		t.Fatal(ok, err)
	}
}
func setManualIncident(t *testing.T, d *replicaDomain, manual bool) {
	t.Helper()
	if err := d.update(context.Background(), func(m *memberRecord) {
		m.Manual = manual
		m.Ready = true
		if manual {
			m.State = "manual_required"
			m.Reason = "recovery_deadline_exceeded"
		} else {
			m.State = "healthy"
			m.Reason = ""
			m.ReplicationStatus = "caught_up"
		}
	}); err != nil {
		t.Fatal(err)
	}
}
func TestRecoveryCoordinatorRebuildErasesOnlyInactiveAndRetainsTerminal(t *testing.T) {
	ctx := context.Background()
	dbs, d, now := recoveryFixture(t)
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('kept','source')")
	if _, err := d.target.current.writer.ExecContext(ctx, "INSERT INTO settings VALUES('discarded','target')"); err != nil {
		t.Fatal(err)
	}
	preview, err := dbs.PreviewRecovery(ctx, "local", RecoveryRequest{MainDomain, "rebuild", "standby"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dbs.AcceptRecovery(ctx, "local", preview.OperationID, preview.Confirmation, false); err != nil {
		t.Fatal(err)
	}
	if err = d.coordinatorStep(ctx); err != nil {
		t.Fatal(err)
	}
	operation, err := dbs.RecoveryOperation(ctx, "local", preview.OperationID)
	if err != nil || operation.Status != "succeeded" || operation.RowsCopied < 1 {
		t.Fatal(operation, err)
	}
	for _, db := range []*Database{d.source, d.target} {
		var value string
		if err = db.current.reader.QueryRowContext(ctx, "SELECT value FROM settings WHERE `key`='kept'").Scan(&value); err != nil || value != "source" {
			t.Fatal(value, err)
		}
	}
	var n int
	if err = d.target.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM settings WHERE `key`='discarded'").Scan(&n); err != nil || n != 0 {
		t.Fatal("target branch survived", n, err)
	}
	if _, err = d.target.current.writer.ExecContext(ctx, "INSERT INTO settings VALUES('after','must remain')"); err != nil {
		t.Fatal(err)
	}
	if _, err = dbs.AcceptRecovery(ctx, "local", preview.OperationID, preview.Confirmation, false); err != nil {
		t.Fatal(err)
	}
	if err = d.target.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM settings WHERE `key`='after'").Scan(&n); err != nil || n != 1 {
		t.Fatal("terminal resubmit rebuilt again", n, err)
	}
	r, err := openControl(ctx, d.control.dataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.control.Close()
	r.clock = d.control.clock
	restored := &Databases{replication: r}
	operation, err = restored.RecoveryOperation(ctx, "local", preview.OperationID)
	if err != nil || operation.Status != "succeeded" {
		t.Fatal("terminal was not durable", operation, err)
	}
	*now = now.Add(30 * 24 * time.Hour)
	if _, err = restored.RecoveryOperation(ctx, "local", preview.OperationID); !errors.Is(err, ErrRecoveryNotFound) {
		t.Fatal("expired operation remained visible", err)
	}
}
func TestRecoveryResumeIsBoundToOneIncident(t *testing.T) {
	ctx := context.Background()
	dbs, d, _ := recoveryFixture(t)
	setManualIncident(t, d, true)
	preview, err := dbs.PreviewRecovery(ctx, "local", RecoveryRequest{MainDomain, "resume", "standby"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dbs.AcceptRecovery(ctx, "local", preview.OperationID, preview.Confirmation, false); err != nil {
		t.Fatal(err)
	}
	next := uuid.NewString()
	if err = d.update(ctx, func(m *memberRecord) { m.Incident = next }); err != nil {
		t.Fatal(err)
	}
	if err = d.coordinatorStep(ctx); err != nil {
		t.Fatal(err)
	}
	op, err := dbs.RecoveryOperation(ctx, "local", preview.OperationID)
	if err != nil || op.Status != "failed" || op.ErrorCode != "storage_confirmation_stale" {
		t.Fatal(op, err)
	}
	m := d.member()
	if !m.Manual || m.Incident != next || m.ResumeIncident != "" {
		t.Fatal("authorization leaked into the next incident", m.Manual, m.Incident, m.ResumeIncident)
	}
}
func TestRecoveryManualPromoteRequiresCompleteRegisteredCandidate(t *testing.T) {
	ctx := context.Background()
	dbs, d, _ := recoveryFixture(t)
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('lost_tail','accepted loss')")
	if err := d.freeze(ctx, "active_unreachable"); err != nil {
		t.Fatal(err)
	}
	preview, err := dbs.PreviewRecovery(ctx, "local", RecoveryRequest{MainDomain, "promote", "standby"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dbs.AcceptRecovery(ctx, "local", preview.OperationID, preview.Confirmation, true); err != nil {
		t.Fatal(err)
	}
	if err = d.coordinatorStep(ctx); err != nil {
		t.Fatal(err)
	}
	operation, err := dbs.RecoveryOperation(ctx, "local", preview.OperationID)
	if err != nil || operation.Status != "succeeded" {
		t.Fatal(operation, err)
	}
	m := d.member()
	if m.Active != "standby" || !d.source.Ready() {
		t.Fatal("candidate was not published", m.Active)
	}
	var n int
	if err = d.source.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM settings WHERE `key`='lost_tail'").Scan(&n); err != nil || n != 0 {
		t.Fatal("old exclusive branch entered new authority", n, err)
	}
}
func TestOutageRateConcurrencyAndFiveAttemptBackoff(t *testing.T) {
	t.Run("rate_and_concurrency", func(t *testing.T) {
		ctx := context.Background()
		dbs, d, now := recoveryFixture(t)
		installOutageChannel(t, dbs)
		for range 12 {
			setManualIncident(t, d, true)
			setManualIncident(t, d, false)
		}
		for batch := range 5 {
			deliveries, err := dbs.ClaimOutageDeliveries(ctx, *now, 100)
			if err != nil || len(deliveries) != 4 {
				t.Fatal(batch, len(deliveries), err)
			}
			extra, err := dbs.ClaimOutageDeliveries(ctx, *now, 4)
			if err != nil || len(extra) != 0 {
				t.Fatal("more than four claims", len(extra), err)
			}
			for _, delivery := range deliveries {
				if err = dbs.FinishOutageDelivery(ctx, delivery.ID, OutageResult{OK: true}, *now); err != nil {
					t.Fatal(err)
				}
			}
		}
		deliveries, err := dbs.ClaimOutageDeliveries(ctx, *now, 4)
		if err != nil || len(deliveries) != 0 {
			t.Fatal("exceeded twenty starts per minute", len(deliveries), err)
		}
		*now = now.Add(time.Minute)
		deliveries, err = dbs.ClaimOutageDeliveries(ctx, *now, 4)
		if err != nil || len(deliveries) != 4 {
			t.Fatal("window boundary remained locked", len(deliveries), err)
		}
	})
	t.Run("attempts", func(t *testing.T) {
		ctx := context.Background()
		dbs, d, now := recoveryFixture(t)
		installOutageChannel(t, dbs)
		setManualIncident(t, d, true)
		delays := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
		for attempt := 1; attempt <= 5; attempt++ {
			deliveries, err := dbs.ClaimOutageDeliveries(ctx, *now, 1)
			if err != nil || len(deliveries) != 1 || deliveries[0].Attempt != attempt {
				t.Fatal(attempt, deliveries, err)
			}
			if err = dbs.FinishOutageDelivery(ctx, deliveries[0].ID, OutageResult{Retry: true, ErrorCode: "delivery_failed"}, *now); err != nil {
				t.Fatal(err)
			}
			if attempt < 5 {
				*now = now.Add(delays[attempt-1] - time.Nanosecond)
				early, err := dbs.ClaimOutageDeliveries(ctx, *now, 1)
				if err != nil || len(early) != 0 {
					t.Fatal("early retry", attempt, early, err)
				}
				*now = now.Add(time.Nanosecond)
			}
		}
		*now = now.Add(24 * time.Hour)
		deliveries, err := dbs.ClaimOutageDeliveries(ctx, *now, 1)
		if err != nil || len(deliveries) != 0 {
			t.Fatal("sixth attempt", deliveries, err)
		}
	})
}
func TestOutagePendingBoundProtectsCurrentManualAndPreferencesStayPaused(t *testing.T) {
	ctx := context.Background()
	dbs, d, now := recoveryFixture(t)
	installOutageChannel(t, dbs)
	setManualIncident(t, d, true)
	current := d.member().Incident
	r := d.control
	r.mu.Lock()
	r.record.Domains[HistoryDomain] = &memberRecord{Active: "primary", State: "healthy", Ready: true, Generation: 1}
	err := r.persistLocked(ctx)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	history := &replicaDomain{control: r, domain: HistoryDomain}
	for range 60 {
		setManualIncident(t, history, true)
		setManualIncident(t, history, false)
	}
	r.mu.Lock()
	pending, protected := 0, false
	for _, delivery := range r.record.Outage.Deliveries {
		if delivery.ChannelID == "channel" && (delivery.Status == "pending" || delivery.Status == "running") {
			pending++
			event, _ := outageEvent(r.record.Outage.Events, delivery.EventID)
			if event.IncidentID == current && event.Transition == "manual_required" {
				protected = true
			}
		}
	}
	r.mu.Unlock()
	if pending != 100 || !protected {
		t.Fatal("outbox lost bound/current manual", pending, protected)
	}
	token, err := dbs.PauseOutageSnapshot(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := dbs.PublishOutagePreferences(ctx, token, StorageNotificationPreferences{Inbox: false, ChannelIDs: []string{}}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	snapshot, err := dbs.OutageSnapshot(ctx)
	if err != nil || snapshot.Ready || snapshot.Preferences.Inbox || len(snapshot.Channels) != 0 {
		t.Fatal(snapshot, err)
	}
	deliveries, err := dbs.ClaimOutageDeliveries(ctx, *now, 4)
	if err != nil || len(deliveries) != 0 {
		t.Fatal("revoked cache dispatched", len(deliveries), err)
	}
}
func TestRecoveryTerminalRetentionIsBounded(t *testing.T) {
	ctx := context.Background()
	dbs, d, now := recoveryFixture(t)
	r := d.control
	r.mu.Lock()
	r.record.Operations = map[string]*recoveryOperationRecord{}
	for i := range 1001 {
		id := fmt.Sprintf("operation-%04d", i)
		phase := "done"
		r.record.Operations[id] = &recoveryOperationRecord{RecoveryOperation: RecoveryOperation{ID: id, Status: "succeeded", Phase: &phase}, Actor: "local", Domain: MainDomain, FinishedAt: now.Add(time.Duration(i-1001) * time.Second)}
	}
	err := r.persistLocked(ctx)
	count := len(r.record.Operations)
	r.mu.Unlock()
	if err != nil || count != 1000 {
		t.Fatal(count, err)
	}
	if _, err = dbs.RecoveryOperation(ctx, "local", "operation-0000"); !errors.Is(err, ErrRecoveryNotFound) {
		t.Fatal("oldest terminal retained", err)
	}
	if _, err = dbs.RecoveryOperation(ctx, "local", "operation-1000"); err != nil {
		t.Fatal("newest terminal evicted", err)
	}
}
func TestOutageLegacySQLQuotaFloorAndRestartShareTwentyStarts(t *testing.T) {
	ctx := context.Background()
	dbs, d, now := recoveryFixture(t)
	installOutageChannel(t, dbs)
	setManualIncident(t, d, true)
	window := now.Add(-10 * time.Second)
	if configured, err := dbs.SeedNotificationRate(ctx, "channel", window, 19, *now); err != nil || !configured {
		t.Fatal(configured, err)
	}
	deliveries, err := dbs.ClaimOutageDeliveries(ctx, *now, 1)
	if err != nil || len(deliveries) != 1 {
		t.Fatal("twentieth shared start rejected", len(deliveries), err)
	}
	if allowed, _, configured, err := dbs.ReserveNotificationRate(ctx, "channel", *now); err != nil || !configured || allowed {
		t.Fatal("twenty-first shared start allowed", allowed, configured, err)
	}
	if _, err = dbs.SeedNotificationRate(ctx, "channel", window, 19, *now); err != nil {
		t.Fatal(err)
	}
	r, err := openControl(ctx, d.control.dataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.control.Close()
	r.clock = d.control.clock
	reopened := &Databases{replication: r}
	if _, err = reopened.SeedNotificationRate(ctx, "channel", window, 19, *now); err != nil {
		t.Fatal(err)
	}
	if allowed, _, configured, err := reopened.ReserveNotificationRate(ctx, "channel", *now); err != nil || !configured || allowed {
		t.Fatal("restart forgot shared quota", allowed, configured, err)
	}
	if len(r.record.Outage.Starts["channel"]) != 20 {
		t.Fatal("same SQL floor imported twice", len(r.record.Outage.Starts["channel"]))
	}
	*now = now.Add(time.Minute)
	if allowed, _, configured, err := reopened.ReserveNotificationRate(ctx, "channel", *now); err != nil || !configured || !allowed {
		t.Fatal("expired SQL floor was not released", allowed, configured, err)
	}
	if len(r.record.Outage.Floors) != 0 {
		t.Fatal("expired SQL floor bookkeeping remained")
	}
}
