package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T) (*Databases, *replicaDomain, *time.Time) {
	t.Helper()
	d, _ := failoverPair(t)
	now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	d.control.clock = func() (string, int64, time.Time, error) { return "boot", int64(time.Hour), now, nil }
	d.control.record.Domains[MainDomain].PrimaryReachable = true
	d.control.record.Domains[MainDomain].StandbyReachable = true
	if err := d.control.persistLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &Databases{Main: d.source, replication: d.control}, d, &now
}
func TestRecoveryPreviewBindingAndIdempotentQueue(t *testing.T) {
	ctx := context.Background()
	dbs, d, now := recoveryFixture(t)
	req := RecoveryRequest{Domain: MainDomain, Action: "rebuild", Target: "standby"}
	p, err := dbs.PreviewRecovery(ctx, "alice", req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Confirmation != "main/standby/1" || p.ExpiresAt.Sub(*now) != 5*time.Minute {
		t.Fatalf("wrong confirmation/expiry: %+v", p)
	}
	if _, err = dbs.AcceptRecovery(ctx, "bob", p.OperationID, p.Confirmation, false); !errors.Is(err, ErrRecoveryNotFound) {
		t.Fatal("cross actor accepted", err)
	}
	if _, err = dbs.AcceptRecovery(ctx, "alice", p.OperationID, "main/primary/0", false); !errors.Is(err, ErrConfirmationStale) {
		t.Fatal("wrong direction accepted", err)
	}
	d.control.mu.Lock()
	d.control.record.Domains[MainDomain].Epoch = "changed"
	d.control.record.Revision++
	err = d.control.persistLocked(ctx)
	d.control.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dbs.AcceptRecovery(ctx, "alice", p.OperationID, p.Confirmation, false); !errors.Is(err, ErrConfirmationStale) {
		t.Fatal("cross epoch accepted", err)
	}
	p, err = dbs.PreviewRecovery(ctx, "alice", req)
	if err != nil {
		t.Fatal(err)
	}
	op, err := dbs.AcceptRecovery(ctx, "alice", p.OperationID, p.Confirmation, false)
	if err != nil || op.Status != "queued" {
		t.Fatal("not queued", op, err)
	}
	again, err := dbs.AcceptRecovery(ctx, "alice", p.OperationID, p.Confirmation, false)
	if err != nil || again.ID != op.ID || again.Status != "queued" {
		t.Fatal("not idempotent", again, err)
	}
	if _, err = dbs.PreviewRecovery(ctx, "alice", req); !errors.Is(err, ErrRecoveryBusy) {
		t.Fatal("queued operation replaced", err)
	}
}
func TestRecoveryPreviewExpiryActiveTargetAndLossGate(t *testing.T) {
	ctx := context.Background()
	dbs, d, now := recoveryFixture(t)
	if _, err := dbs.PreviewRecovery(ctx, "alice", RecoveryRequest{MainDomain, "rebuild", "primary"}); !errors.Is(err, ErrManualRequired) {
		t.Fatal("active rebuild accepted", err)
	}
	p, err := dbs.PreviewRecovery(ctx, "alice", RecoveryRequest{MainDomain, "rebuild", "standby"})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(5 * time.Minute)
	if _, err = dbs.AcceptRecovery(ctx, "alice", p.OperationID, p.Confirmation, false); !errors.Is(err, ErrRecoveryNotFound) {
		t.Fatal("expired preview accepted", err)
	}
	if err = d.freeze(ctx, "active_unreachable"); err != nil {
		t.Fatal(err)
	}
	p, err = dbs.PreviewRecovery(ctx, "alice", RecoveryRequest{MainDomain, "promote", "standby"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.RequiresDataLossAcceptance {
		t.Fatal("promotion omitted explicit loss gate")
	}
	if _, err = dbs.AcceptRecovery(ctx, "alice", p.OperationID, p.Confirmation, false); !errors.Is(err, ErrManualRequired) {
		t.Fatal("loss not accepted", err)
	}
}
func TestOutageSnapshotCASDeliveryAndGenerationReplay(t *testing.T) {
	ctx := context.Background()
	dbs, d, now := recoveryFixture(t)
	older, err := dbs.PauseOutageSnapshot(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := dbs.PauseOutageSnapshot(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	prefs := StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{"channel"}}
	channels := []OutageChannel{{ID: "channel", Ciphertext: []byte("encrypted")}}
	if ok, err := dbs.PublishOutageSnapshot(ctx, older, prefs, channels); err != nil || ok {
		t.Fatal("stale cache published", ok, err)
	}
	if ok, err := dbs.PublishOutageSnapshot(ctx, newer, prefs, channels); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err = d.update(ctx, func(m *memberRecord) {
		m.Manual = true
		m.State = "manual_required"
		m.Reason = "recovery_deadline_exceeded"
	}); err != nil {
		t.Fatal(err)
	}
	deliveries, err := dbs.ClaimOutageDeliveries(ctx, *now, 4)
	if err != nil || len(deliveries) != 1 {
		t.Fatal("missing independent delivery", len(deliveries), err)
	}
	if _, err = dbs.PauseOutageSnapshot(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := dbs.OutageDeliveryCurrent(ctx, deliveries[0].ID, deliveries[0].Token); err != nil || ok {
		t.Fatal("paused delivery valid", ok, err)
	}
	events, err := dbs.OutageProjection(ctx, 1, 10)
	if err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
	if err = dbs.MarkOutageProjected(ctx, events[0].ID, 1); err != nil {
		t.Fatal(err)
	}
	events, err = dbs.OutageProjection(ctx, 1, 10)
	if err != nil || len(events) != 0 {
		t.Fatal("same generation replayed", events, err)
	}
	events, err = dbs.OutageProjection(ctx, 2, 10)
	if err != nil || len(events) != 1 {
		t.Fatal("new generation lost replay", events, err)
	}
	var raw []byte
	if err = d.control.control.QueryRowContext(ctx, "SELECT record FROM storage_control WHERE id=1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var persisted controlRecord
	if err = json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if !persisted.Domains[MainDomain].Manual {
		t.Fatal("event persisted without manual state")
	}
}
func TestRecoveryControlFailureDoesNotPublishEvent(t *testing.T) {
	ctx := context.Background()
	dbs, d, _ := recoveryFixture(t)
	if _, err := d.control.control.ExecContext(ctx, "CREATE TRIGGER reject_e BEFORE UPDATE ON storage_control BEGIN SELECT RAISE(ABORT,'blocked'); END"); err != nil {
		t.Fatal(err)
	}
	if err := d.update(ctx, func(m *memberRecord) { m.Manual = true; m.State = "manual_required" }); err == nil {
		t.Fatal("control failure ignored")
	}
	events, err := dbs.OutageProjection(ctx, 1, 10)
	if err != nil || len(events) != 0 {
		t.Fatal("failed state leaked event", events, err)
	}
}
func TestRecoveryReplacedPreviewCannotQueue(t *testing.T) {
	ctx := context.Background()
	dbs, _, _ := recoveryFixture(t)
	first, err := dbs.PreviewRecovery(ctx, "alice", RecoveryRequest{MainDomain, "rebuild", "standby"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := dbs.PreviewRecovery(ctx, "alice", RecoveryRequest{MainDomain, "rebuild", "standby"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dbs.AcceptRecovery(ctx, "alice", first.OperationID, first.Confirmation, false); !errors.Is(err, ErrConfirmationStale) {
		t.Fatal("superseded preview was accepted", err)
	}
	if _, err = dbs.AcceptRecovery(ctx, "alice", second.OperationID, second.Confirmation, false); err != nil {
		t.Fatal(err)
	}
}
func TestRecoveryFailedPruneRestoresOperationMemoryAndSQL(t *testing.T) {
	ctx := context.Background()
	dbs, d, now := recoveryFixture(t)
	installOutageChannel(t, dbs)
	r := d.control
	r.record.Operations = map[string]*recoveryOperationRecord{"expired": {RecoveryOperation: RecoveryOperation{ID: "expired", Status: "succeeded"}, Domain: MainDomain, Actor: "alice", FinishedAt: *now}}
	if err := r.persistLocked(ctx); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(31 * 24 * time.Hour)
	if _, err := r.control.ExecContext(ctx, "CREATE TRIGGER reject_prune BEFORE UPDATE ON storage_control BEGIN SELECT RAISE(ABORT,'blocked'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := dbs.ClaimOutageDeliveries(ctx, *now, 4); err == nil {
		t.Fatal("control failure was ignored")
	}
	if r.record.Operations["expired"] == nil {
		t.Fatal("failed control transaction pruned only memory")
	}
	var raw []byte
	if err := r.control.QueryRowContext(ctx, "SELECT record FROM storage_control WHERE id=1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var disk controlRecord
	if err := json.Unmarshal(raw, &disk); err != nil || disk.Operations["expired"] == nil {
		t.Fatal("failed prune changed durable operation", err)
	}
}
func TestRecoveryStatusUsesActualACKAndRejectsCrossBootLossProof(t *testing.T) {
	ctx := context.Background()
	dbs, d, now := recoveryFixture(t)
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('proof','durable')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	barrier := d.member().LastBarrier
	if barrier == nil {
		t.Fatal("actual target ACK did not retain its barrier")
	}
	d.control.clock = func() (string, int64, time.Time, error) {
		return barrier.Boot, barrier.Nanos + int64(3*time.Second), *now, nil
	}
	healthy, err := dbs.RecoveryStatus(ctx)
	if err != nil || healthy.Domains[0].ProvenLossSeconds != nil {
		t.Fatal("healthy reported an incident loss proof", err)
	}
	if err = d.freeze(ctx, "active_unreachable"); err != nil {
		t.Fatal(err)
	}
	status, err := dbs.RecoveryStatus(ctx)
	if err != nil || status.Domains[0].ProvenLossSeconds == nil || *status.Domains[0].ProvenLossSeconds != 3 {
		t.Fatal("frozen ACK bound was not three seconds", err)
	}
	if status.Domains[0].ManualDeadlineAt == nil || !status.Domains[0].ManualDeadlineAt.Equal(now.Add(24*time.Hour)) {
		t.Fatal("manual deadline was not an absolute time")
	}
	d.control.clock = func() (string, int64, time.Time, error) {
		return "different-boot", barrier.Nanos + int64(3*time.Second), *now, nil
	}
	status, err = dbs.RecoveryStatus(ctx)
	if err != nil || status.Domains[0].ProvenLossSeconds != nil {
		t.Fatal("cross-boot proof was presented as verified", err)
	}
}
func TestRecoveryResumeClearsOnlyAfterRealTargetACK(t *testing.T) {
	ctx := context.Background()
	dbs, d, _ := recoveryFixture(t)
	setManualIncident(t, d, true)
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('resumed','retained')")
	preview, err := dbs.PreviewRecovery(ctx, "local", RecoveryRequest{MainDomain, "resume", "standby"})
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
	if err != nil || operation.Status != "succeeded" {
		t.Fatal(operation, err)
	}
	m := d.member()
	if m.Manual || m.Incident != "" || m.ResumeIncident != "" {
		t.Fatal("completed incident retained authorization/lock", m.Manual, m.Incident, m.ResumeIncident)
	}
	var value string
	if err = d.target.current.reader.QueryRowContext(ctx, "SELECT value FROM settings WHERE `key`='resumed'").Scan(&value); err != nil || value != "retained" {
		t.Fatal("manual lock cleared without a real target ACK", err)
	}
}
func TestRecoveryRestoreDoesNotDiscardQueuedOperation(t *testing.T) {
	ctx := context.Background()
	dbs, d, _ := recoveryFixture(t)
	preview, err := dbs.PreviewRecovery(ctx, "local", RecoveryRequest{MainDomain, "rebuild", "standby"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dbs.AcceptRecovery(ctx, "local", preview.OperationID, preview.Confirmation, false); err != nil {
		t.Fatal(err)
	}
	if err = dbs.BeginRestore(ctx, "restore-operation", []Domain{MainDomain}); !errors.Is(err, ErrRecoveryBusy) {
		t.Fatal("restore displaced an accepted recovery", err)
	}
	operation, err := dbs.RecoveryOperation(ctx, "local", preview.OperationID)
	if err != nil || operation.Status != "queued" || d.control.record.Pending != "" {
		t.Fatal(operation, err)
	}
}
