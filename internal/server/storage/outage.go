package storage

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"
)

type StorageNotificationPreferences struct {
	Inbox      bool     `json:"inbox"`
	ChannelIDs []string `json:"channel_ids"`
}
type StorageNotificationSettings struct {
	StorageNotificationPreferences
	SnapshotReady bool `json:"snapshot_ready"`
}
type OutageSnapshotToken struct{ Generation, Revision uint64 }
type OutageChannel struct {
	ID         string
	Ciphertext []byte
}
type OutageSnapshot struct {
	Ready       bool
	Preferences StorageNotificationPreferences
	Channels    []OutageChannel
}
type OutageEvent struct {
	ID, IncidentID                     string
	Domain                             Domain
	Transition, ActiveRole, ReasonCode string
	OccurredAt                         time.Time
}
type OutageDelivery struct {
	ID      string
	Event   OutageEvent
	Channel OutageChannel
	Attempt int
	Token   OutageSnapshotToken
}
type OutageResult struct {
	OK         bool
	ErrorCode  string
	Retry      bool
	RetryAfter time.Duration
}
type outageEventRecord struct {
	Event               OutageEvent
	Inbox               bool
	ProjectedGeneration uint64
	InboxGeneration     uint64
	FanoutGeneration    uint64
	ChannelIDs          []string
	KeepUntilProjected  bool
}
type outageDeliveryRecord struct {
	ID, EventID, ChannelID, Status, ClaimID, ErrorCode string
	Attempt                                            int
	NextAttempt, LeaseUntil, FinishedAt                time.Time
	Token                                              OutageSnapshotToken
}
type outageControl struct {
	Configured       bool
	Snapshot         OutageSnapshot
	Token            OutageSnapshotToken
	PreferencesToken OutageSnapshotToken
	Events           []outageEventRecord
	Deliveries       []outageDeliveryRecord
	Ended            map[string]time.Time
	Starts           map[string][]time.Time
	Floors           map[string]notificationRateFloor
}

func outagePreferences(p StorageNotificationPreferences) StorageNotificationPreferences {
	if p.ChannelIDs == nil {
		p.ChannelIDs = []string{}
	}
	return p
}
func (r *replication) defaultOutage() {
	if !r.record.Outage.Configured {
		r.record.Outage.Snapshot.Preferences = StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{}}
		r.record.Outage.Configured = true
	}
}
func copyOutage(v outageControl) outageControl {
	b, _ := json.Marshal(v)
	var copy outageControl
	_ = json.Unmarshal(b, &copy)
	return copy
}
func cloneSnapshot(v OutageSnapshot) OutageSnapshot {
	v.Preferences.ChannelIDs = append([]string{}, v.Preferences.ChannelIDs...)
	channels := make([]OutageChannel, len(v.Channels))
	for i, c := range v.Channels {
		channels[i] = OutageChannel{ID: c.ID, Ciphertext: append([]byte(nil), c.Ciphertext...)}
	}
	v.Channels = channels
	return v
}
func (dbs *Databases) OutageSnapshot(ctx context.Context) (OutageSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return OutageSnapshot{}, err
	}
	if dbs == nil || dbs.replication == nil {
		return OutageSnapshot{Preferences: StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{}}, Channels: []OutageChannel{}}, nil
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultOutage()
	snapshot := cloneSnapshot(r.record.Outage.Snapshot)
	m := r.record.Domains[MainDomain]
	snapshot.Ready = snapshot.Ready && !r.record.Disabled && m != nil && r.record.Outage.Token.Generation == m.Generation
	return snapshot, nil
}
func (dbs *Databases) PauseOutageSnapshot(ctx context.Context, generation uint64) (OutageSnapshotToken, error) {
	if dbs == nil || dbs.replication == nil {
		return OutageSnapshotToken{}, nil
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultOutage()
	m := r.record.Domains[MainDomain]
	if m == nil || generation == 0 || generation != m.Generation {
		return OutageSnapshotToken{}, ErrConfirmationStale
	}
	old := copyOutage(r.record.Outage)
	r.record.Outage.Token.Revision++
	r.record.Outage.Token.Generation = generation
	r.record.Outage.Snapshot.Ready = false
	token := r.record.Outage.Token
	if err := r.persistLocked(ctx); err != nil {
		r.record.Outage = old
		return OutageSnapshotToken{}, err
	}
	return token, nil
}

// PublishOutagePreferences 不解除发送暂停；SQL 已确认的订阅先独立发布。
func (dbs *Databases) PublishOutagePreferences(ctx context.Context, token OutageSnapshotToken, prefs StorageNotificationPreferences) (bool, error) {
	if dbs == nil || dbs.replication == nil {
		return false, nil
	}
	if len(prefs.ChannelIDs) > 20 {
		return false, ErrManualRequired
	}
	ids := map[string]bool{}
	for _, id := range prefs.ChannelIDs {
		if id == "" || ids[id] {
			return false, ErrManualRequired
		}
		ids[id] = true
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.record.Domains[MainDomain]
	if m == nil || token.Generation != m.Generation || token != r.record.Outage.Token {
		return false, nil
	}
	old := copyOutage(r.record.Outage)
	o := &r.record.Outage
	o.Configured = true
	o.Snapshot.Ready = false
	prefs.ChannelIDs = append([]string{}, prefs.ChannelIDs...)
	o.Snapshot.Preferences = outagePreferences(prefs)
	channels := o.Snapshot.Channels[:0]
	for _, c := range o.Snapshot.Channels {
		if ids[c.ID] {
			channels = append(channels, c)
		}
	}
	o.Snapshot.Channels = channels
	now, err := r.now()
	if err != nil {
		r.record.Outage = old
		return false, err
	}
	for i := range o.Deliveries {
		v := &o.Deliveries[i]
		if !ids[v.ChannelID] && (v.Status == "pending" || v.Status == "running") {
			v.Status = "cancelled"
			v.FinishedAt = now.Wall
		}
	}
	r.calibrateOutagePreferencesLocked(token, prefs, ids)
	if err = r.persistLocked(ctx); err != nil {
		r.record.Outage = old
		return false, err
	}
	return true, nil
}
func (dbs *Databases) PublishOutageSnapshot(ctx context.Context, token OutageSnapshotToken, prefs StorageNotificationPreferences, channels []OutageChannel) (bool, error) {
	if dbs == nil || dbs.replication == nil {
		return false, nil
	}
	if len(prefs.ChannelIDs) > 20 || len(channels) > 20 {
		return false, ErrManualRequired
	}
	ids := map[string]bool{}
	for _, id := range prefs.ChannelIDs {
		if id == "" || ids[id] {
			return false, ErrManualRequired
		}
		ids[id] = true
	}
	seen := map[string]bool{}
	for _, c := range channels {
		if !ids[c.ID] || seen[c.ID] || len(c.Ciphertext) == 0 || len(c.Ciphertext) > 1<<20 {
			return false, ErrManualRequired
		}
		seen[c.ID] = true
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.record.Domains[MainDomain]
	if m == nil || token.Generation != m.Generation || token != r.record.Outage.Token {
		return false, nil
	}
	old := copyOutage(r.record.Outage)
	r.record.Outage.Configured = true
	r.record.Outage.Snapshot = cloneSnapshot(OutageSnapshot{Ready: true, Preferences: outagePreferences(prefs), Channels: channels})
	now, err := r.now()
	if err != nil {
		r.record.Outage = old
		return false, err
	}
	for i := range r.record.Outage.Deliveries {
		delivery := &r.record.Outage.Deliveries[i]
		if !seen[delivery.ChannelID] && (delivery.Status == "pending" || delivery.Status == "running") {
			delivery.Status = "cancelled"
			delivery.FinishedAt = now.Wall
		}
	}
	r.calibrateOutagePreferencesLocked(token, prefs, ids)
	for i := range r.record.Outage.Events {
		e := &r.record.Outage.Events[i]
		if e.FanoutGeneration != token.Generation {
			continue
		}
		channelIDs := make([]string, 0, len(e.ChannelIDs))
		for _, id := range e.ChannelIDs {
			if seen[id] {
				channelIDs = append(channelIDs, id)
			}
		}
		e.FanoutGeneration = 0
		e.ChannelIDs = nil
		r.fanoutOutageLocked(e.Event, channelIDs, now.Wall)
	}
	if err = r.persistLocked(ctx); err != nil {
		r.record.Outage = old
		return false, err
	}
	return true, nil
}

func (r *replication) calibrateOutagePreferencesLocked(token OutageSnapshotToken, prefs StorageNotificationPreferences, selected map[string]bool) {
	o := &r.record.Outage
	o.PreferencesToken = token
	for i := range o.Events {
		e := &o.Events[i]
		if e.InboxGeneration == token.Generation {
			e.Inbox = prefs.Inbox
			e.InboxGeneration = 0
		}
		if e.FanoutGeneration != token.Generation {
			continue
		}
		if e.ChannelIDs == nil {
			e.ChannelIDs = append([]string{}, prefs.ChannelIDs...)
			continue
		}
		kept := e.ChannelIDs[:0]
		for _, id := range e.ChannelIDs {
			if selected[id] {
				kept = append(kept, id)
			}
		}
		e.ChannelIDs = kept
	}
}
func outageChannel(channels []OutageChannel, id string) (OutageChannel, bool) {
	for _, c := range channels {
		if c.ID == id {
			return c, true
		}
	}
	return OutageChannel{}, false
}
func outageEvent(events []outageEventRecord, id string) (OutageEvent, bool) {
	for _, e := range events {
		if e.Event.ID == id {
			return e.Event, true
		}
	}
	return OutageEvent{}, false
}
func (dbs *Databases) ClaimOutageDeliveries(ctx context.Context, now time.Time, limit int) ([]OutageDelivery, error) {
	result := []OutageDelivery{}
	if dbs == nil || dbs.replication == nil || limit <= 0 {
		return result, nil
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.record.Domains[MainDomain]
	if !r.record.Outage.Snapshot.Ready || r.record.Disabled || m == nil || r.record.Outage.Token.Generation != m.Generation {
		return result, nil
	}
	if limit > 4 {
		limit = 4
	}
	old := copyOutage(r.record.Outage)
	o := &r.record.Outage
	active := 0
	for i := range o.Deliveries {
		v := &o.Deliveries[i]
		if v.Status == "running" {
			if now.Before(v.LeaseUntil) {
				active++
			} else if v.Attempt >= 5 {
				v.Status = "failed"
				v.FinishedAt = now
			} else {
				v.Status = "pending"
				v.NextAttempt = v.LeaseUntil.Add(outageBackoffs[v.Attempt-1])
			}
		}
	}
	if o.Starts == nil {
		o.Starts = map[string][]time.Time{}
	}
	for i := range o.Deliveries {
		if active >= 4 || len(result) >= limit {
			break
		}
		v := &o.Deliveries[i]
		if v.Status != "pending" || now.Before(v.NextAttempt) || v.Attempt >= 5 {
			continue
		}
		channel, ok := outageChannel(o.Snapshot.Channels, v.ChannelID)
		if !ok {
			continue
		}
		event, ok := outageEvent(o.Events, v.EventID)
		if !ok {
			continue
		}
		if _, ended := o.Ended[event.IncidentID]; ended && event.OccurredAt.Before(now.Add(-30*24*time.Hour)) {
			continue
		}
		if allowed, _ := o.reserveRate(v.ChannelID, now); !allowed {
			continue
		}
		v.Attempt++
		v.Status = "running"
		v.ClaimID = uuid.NewString()
		v.LeaseUntil = now.Add(30 * time.Second)
		v.Token = o.Token
		result = append(result, OutageDelivery{ID: v.ClaimID, Event: event, Channel: OutageChannel{ID: channel.ID, Ciphertext: append([]byte(nil), channel.Ciphertext...)}, Attempt: v.Attempt, Token: v.Token})
		active++
	}
	if err := r.persistLocked(ctx); err != nil {
		r.record.Outage = old
		return nil, err
	}
	return result, nil
}
func (dbs *Databases) OutageDeliveryCurrent(ctx context.Context, id string, token OutageSnapshotToken) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if dbs == nil || dbs.replication == nil {
		return false, nil
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	o := &r.record.Outage
	m := r.record.Domains[MainDomain]
	if !o.Snapshot.Ready || o.Token != token || r.record.Disabled || m == nil || m.Generation != token.Generation {
		return false, nil
	}
	now, err := r.now()
	if err != nil {
		return false, err
	}
	for _, v := range o.Deliveries {
		if v.ClaimID == id && v.Status == "running" && v.Token == token && now.Wall.Before(v.LeaseUntil) {
			_, ok := outageChannel(o.Snapshot.Channels, v.ChannelID)
			return ok, nil
		}
	}
	return false, nil
}
func (dbs *Databases) FinishOutageDelivery(ctx context.Context, id string, result OutageResult, now time.Time) error {
	if dbs == nil || dbs.replication == nil {
		return nil
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.record.Outage.Deliveries {
		v := &r.record.Outage.Deliveries[i]
		if v.ClaimID != id || v.Status != "running" {
			continue
		}
		old := copyOutage(r.record.Outage)
		v.ErrorCode = stableOutageError(result.ErrorCode)
		if result.OK {
			v.Status = "succeeded"
			v.FinishedAt = now
		} else if !result.Retry || v.Attempt >= 5 {
			v.Status = "failed"
			v.FinishedAt = now
		} else {
			delay := outageBackoffs[v.Attempt-1]
			if result.RetryAfter > delay {
				delay = result.RetryAfter
			}
			v.Status = "pending"
			v.NextAttempt = now.Add(delay)
		}
		if err := r.persistLocked(ctx); err != nil {
			r.record.Outage = old
			return err
		}
		return nil
	}
	return nil
}

// 只保留消费者提供的固定错误码，绝不保存服务端响应或密钥。
func stableOutageError(code string) string {
	if len(code) > 80 {
		return "delivery_failed"
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' {
			return "delivery_failed"
		}
	}
	return code
}
func (dbs *Databases) OutageProjection(ctx context.Context, generation uint64, limit int) ([]OutageEvent, error) {
	result := []OutageEvent{}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dbs == nil || dbs.replication == nil || generation == 0 || limit <= 0 {
		return result, nil
	}
	if limit > 1000 {
		limit = 1000
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	now, err := r.now()
	if err != nil {
		return nil, err
	}
	for _, e := range r.record.Outage.Events {
		_, ended := r.record.Outage.Ended[e.Event.IncidentID]
		retained := !ended || !e.Event.OccurredAt.Before(now.Wall.Add(-30*24*time.Hour)) || e.KeepUntilProjected && r.needsOutageProjectionLocked(e)
		if e.Inbox && e.InboxGeneration == 0 && e.ProjectedGeneration != generation && retained {
			result = append(result, e.Event)
			if len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}
func (dbs *Databases) MarkOutageProjected(ctx context.Context, id string, generation uint64) error {
	if dbs == nil || dbs.replication == nil {
		return nil
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.record.Outage.Events {
		e := &r.record.Outage.Events[i]
		if e.Event.ID == id {
			old := copyOutage(r.record.Outage)
			if e.ProjectedGeneration < generation {
				e.ProjectedGeneration = generation
			}
			if err := r.persistLocked(ctx); err != nil {
				r.record.Outage = old
				return err
			}
			return nil
		}
	}
	return nil
}
func (r *replication) addOutageLocked(m *memberRecord, domain Domain, transition string, now time.Time) {
	if m.Incident == "" {
		m.Incident = uuid.NewString()
	}
	o := &r.record.Outage
	for _, e := range o.Events {
		if e.Event.IncidentID == m.Incident && e.Event.Transition == transition {
			return
		}
	}
	event := OutageEvent{ID: uuid.NewString(), IncidentID: m.Incident, Domain: domain, Transition: transition, ActiveRole: m.Active, ReasonCode: stableOutageError(m.Reason), OccurredAt: now}
	generation := o.Token.Generation
	if main := r.record.Domains[MainDomain]; main != nil {
		generation = main.Generation
	}
	known := o.PreferencesToken == o.Token && o.Token.Generation == generation && o.Token.Revision != 0
	record := outageEventRecord{Event: event}
	if known {
		record.Inbox = o.Snapshot.Preferences.Inbox
	} else {
		record.InboxGeneration = generation
	}
	if !o.Snapshot.Ready || o.Token.Generation != generation {
		record.FanoutGeneration = generation
		if known {
			record.ChannelIDs = append([]string{}, o.Snapshot.Preferences.ChannelIDs...)
		}
		o.Events = append(o.Events, record)
		return
	}
	o.Events = append(o.Events, record)
	channelIDs := make([]string, 0, len(o.Snapshot.Channels))
	for _, channel := range o.Snapshot.Channels {
		channelIDs = append(channelIDs, channel.ID)
	}
	r.fanoutOutageLocked(event, channelIDs, now)
}

func (r *replication) fanoutOutageLocked(event OutageEvent, channelIDs []string, now time.Time) {
	o := &r.record.Outage
	for _, channelID := range channelIDs {
		duplicate := false
		for _, delivery := range o.Deliveries {
			if delivery.EventID == event.ID && delivery.ChannelID == channelID {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		pending := 0
		evict := -1
		for i, d := range o.Deliveries {
			if d.ChannelID != channelID || (d.Status != "pending" && d.Status != "running") {
				continue
			}
			pending++
			e, ok := outageEvent(o.Events, d.EventID)
			_, ended := o.Ended[e.IncidentID]
			if ok && ended && d.Status == "pending" && (evict < 0 || e.OccurredAt.Before(outageEventTime(o.Events, o.Deliveries[evict].EventID))) {
				evict = i
			}
		}
		if pending >= 100 {
			if evict < 0 {
				continue
			}
			o.Deliveries = append(o.Deliveries[:evict], o.Deliveries[evict+1:]...)
		}
		o.Deliveries = append(o.Deliveries, outageDeliveryRecord{ID: uuid.NewString(), EventID: event.ID, ChannelID: channelID, Status: "pending", NextAttempt: now})
	}
}
func outageEventTime(events []outageEventRecord, id string) time.Time {
	e, _ := outageEvent(events, id)
	return e.OccurredAt
}
func recoverySignificant(m *memberRecord) string {
	if m == nil {
		return ""
	}
	v := struct {
		Resource, Primary, Standby, Epoch, Active, State, Reason, Incident string
		Ready, PrimaryRegistered, StandbyRegistered, Manual                bool
		Intent                                                             *switchIntent
		Rebuild                                                            *rebuildOperation
		Generation                                                         uint64
	}{m.Resource, m.Primary, m.Standby, m.Epoch, m.Active, m.State, m.Reason, m.Incident, m.Ready, m.PrimaryRegistered, m.StandbyRegistered, m.Manual, m.Intent, m.Rebuild, m.Generation}
	// 重建批次进度不是新的确认前提。
	if v.Rebuild != nil {
		copy := *v.Rebuild
		copy.Phase = ""
		v.Rebuild = &copy
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}
func (r *replication) stageOutagesLocked(previous controlRecord, now time.Time) {
	r.defaultOutage()
	calibrationChanged := false
	if oldMain, newMain := previous.Domains[MainDomain], r.record.Domains[MainDomain]; oldMain != nil && newMain != nil && oldMain.Generation != newMain.Generation {
		r.record.Outage.Snapshot.Ready = false
		r.record.Outage.Token.Generation = newMain.Generation
		r.record.Outage.Token.Revision++
		calibrationChanged = true
	}
	if previous.Pending != r.record.Pending && r.record.Pending != "" {
		r.record.Outage.Snapshot.Ready = false
		r.record.Outage.Token.Revision++
		calibrationChanged = true
	}
	if previous.Finished != r.record.Finished && r.record.Finished != "" {
		r.record.Outage.Snapshot.Ready = false
		r.record.Outage.Token.Revision++
		for i := range r.record.Outage.Events {
			r.record.Outage.Events[i].ProjectedGeneration = 0
		}
		calibrationChanged = true
	}
	if calibrationChanged {
		r.resetOutageCalibrationLocked()
	}
	for _, op := range r.record.Operations {
		m := r.record.Domains[op.Domain]
		if op.Status == "running" && m != nil && m.Intent != nil && m.Intent.Target == op.Target && (op.Action == "promote" || op.Action == "resume") {
			phase := "switching"
			op.Phase = &phase
			op.AppliedEpoch = m.Intent.Epoch
		}
	}
	for domain, m := range r.record.Domains {
		old := previous.Domains[domain]
		if old == nil {
			continue
		}
		if m.Incident == "" && old.Incident != "" {
			m.Incident = old.Incident
		}
		if old.Freeze == nil && m.Freeze != nil && old.recoveryAuthorized() {
			r.endOutageIncidentLocked(old.Incident, now)
			m.Incident = uuid.NewString()
			m.ResumeIncident = ""
			m.Manual = true
			m.State = "manual_required"
		}
		if (old.Freeze == nil && m.Freeze != nil) || (m.ReplicationStatus == "offline" && old.ReplicationStatus != "offline" && (old.Ready || old.LastSyncedAt != nil || old.Active == "standby")) {
			r.addOutageLocked(m, domain, "replica_unavailable", now)
		}
		if m.Manual && (!old.Manual || m.Incident != old.Incident) {
			r.addOutageLocked(m, domain, "manual_required", now)
		}
		if old.Active != "standby" && m.Active == "standby" {
			r.addOutageLocked(m, domain, "standby_activated", now)
		}
		if m.Reason == "recovery_failed" && old.Reason != "recovery_failed" {
			r.addOutageLocked(m, domain, "recovery_failed", now)
		}
		if m.Incident != "" && (m.Rebuild != nil || m.Intent != nil) && ((m.ReplicationStatus == "offline" && old.ReplicationStatus != "offline") || (m.Manual && !old.Manual)) {
			r.addOutageLocked(m, domain, "recovery_failed", now)
		}
		if old.Active == "standby" && m.Active == "primary" && m.Intent == nil {
			r.addOutageLocked(m, domain, "primary_restored", now)
		}
		if old.Intent != nil && old.Intent.Target == "primary" && m.Intent == nil && m.Active == "primary" {
			r.addOutageLocked(m, domain, "primary_restored", now)
		}
		if old.Freeze != nil && m.Freeze == nil && m.Active == "primary" && m.Incident != "" {
			r.addOutageLocked(m, domain, "primary_restored", now)
		}
		if old.Manual && !m.Manual && m.Active == "primary" && m.Incident != "" {
			r.addOutageLocked(m, domain, "primary_restored", now)
		}
		if m.Incident != "" && m.Active == "primary" && m.Freeze == nil && !m.Manual && m.Ready && m.Intent == nil && m.ReplicationStatus == "caught_up" {
			r.endOutageIncidentLocked(m.Incident, now)
			m.Incident = ""
			m.ResumeIncident = ""
		}
	}
	significant := previous.Pending != r.record.Pending || previous.Disabled != r.record.Disabled
	for domain := range r.record.Domains {
		var oldBusy, newBusy string
		for id, op := range previous.Operations {
			if op.Domain == domain && (op.Status == "queued" || op.Status == "running") {
				oldBusy = id
			}
		}
		for id, op := range r.record.Operations {
			if op.Domain == domain && (op.Status == "queued" || op.Status == "running") {
				newBusy = id
			}
		}
		if oldBusy != newBusy {
			significant = true
		}
	}
	for domain, m := range r.record.Domains {
		if recoverySignificant(previous.Domains[domain]) != recoverySignificant(m) {
			significant = true
		}
	}
	if previous.Cluster != "" {
		r.record.Revision = previous.Revision
		if significant {
			r.record.Revision++
		}
	}
	for id, op := range r.record.Operations {
		old := previous.Operations[id]
		if op.Status == "failed" && (old == nil || old.Status != "failed") && op.ErrorCode != "storage_confirmation_stale" {
			if m := r.record.Domains[op.Domain]; m != nil {
				r.addOutageLocked(m, op.Domain, "recovery_failed", now)
			}
		}
	}
	r.pruneRecoveryLocked(now)
}

func (r *replication) resetOutageCalibrationLocked() {
	o := &r.record.Outage
	o.PreferencesToken = OutageSnapshotToken{}
	main := r.record.Domains[MainDomain]
	if main == nil {
		return
	}
	for i := range o.Events {
		e := &o.Events[i]
		if e.InboxGeneration != 0 {
			e.InboxGeneration = main.Generation
		}
		if e.FanoutGeneration != 0 {
			e.FanoutGeneration = main.Generation
			e.ChannelIDs = nil
		}
	}
}

func (r *replication) needsOutageProjectionLocked(event outageEventRecord) bool {
	if event.InboxGeneration != 0 {
		return true
	}
	if !event.Inbox {
		return false
	}
	main := r.record.Domains[MainDomain]
	return event.ProjectedGeneration == 0 || main != nil && event.ProjectedGeneration != main.Generation
}

func (r *replication) endOutageIncidentLocked(id string, now time.Time) {
	o := &r.record.Outage
	if o.Ended == nil {
		o.Ended = map[string]time.Time{}
	}
	for i := range o.Events {
		e := &o.Events[i]
		if e.Event.IncidentID == id && e.Event.OccurredAt.Before(now.Add(-30*24*time.Hour)) && r.needsOutageProjectionLocked(*e) {
			e.KeepUntilProjected = true
		}
	}
	o.Ended[id] = now
}
func (r *replication) pruneRecoveryLocked(now time.Time) {
	for id, op := range r.record.Operations {
		if (op.Status == "awaiting_confirmation" && !now.Before(op.ExpiresAt)) || (!op.FinishedAt.IsZero() && op.FinishedAt.Before(now.Add(-30*24*time.Hour))) {
			delete(r.record.Operations, id)
		}
	}
	var terminal []*recoveryOperationRecord
	for _, op := range r.record.Operations {
		if !op.FinishedAt.IsZero() {
			terminal = append(terminal, op)
		}
	}
	sort.Slice(terminal, func(i, j int) bool { return terminal[i].FinishedAt.Before(terminal[j].FinishedAt) })
	if len(terminal) > 1000 {
		for _, op := range terminal[:len(terminal)-1000] {
			delete(r.record.Operations, op.ID)
		}
	}
	var replaced []*recoveryOperationRecord
	for _, op := range r.record.Operations {
		if op.Replaced {
			replaced = append(replaced, op)
		}
	}
	sort.Slice(replaced, func(i, j int) bool {
		if replaced[i].CreatedAt.Equal(replaced[j].CreatedAt) {
			return replaced[i].ID < replaced[j].ID
		}
		return replaced[i].CreatedAt.Before(replaced[j].CreatedAt)
	})
	if len(replaced) > 1000 {
		for _, op := range replaced[:len(replaced)-1000] {
			delete(r.record.Operations, op.ID)
		}
	}
	o := &r.record.Outage
	deliveries := o.Deliveries[:0]
	var terminalDeliveries []outageDeliveryRecord
	for _, v := range o.Deliveries {
		event, exists := outageEvent(o.Events, v.EventID)
		_, ended := o.Ended[event.IncidentID]
		if !exists || ended && event.OccurredAt.Before(now.Add(-30*24*time.Hour)) {
			continue
		}
		if !v.FinishedAt.IsZero() {
			if !v.FinishedAt.Before(now.Add(-30 * 24 * time.Hour)) {
				terminalDeliveries = append(terminalDeliveries, v)
			}
		} else {
			deliveries = append(deliveries, v)
		}
	}
	sort.Slice(terminalDeliveries, func(i, j int) bool { return terminalDeliveries[i].FinishedAt.Before(terminalDeliveries[j].FinishedAt) })
	if len(terminalDeliveries) > 1000 {
		terminalDeliveries = terminalDeliveries[len(terminalDeliveries)-1000:]
	}
	o.Deliveries = append(deliveries, terminalDeliveries...)
	keep := map[string]bool{}
	for _, v := range o.Deliveries {
		keep[v.EventID] = true
	}
	events := o.Events[:0]
	var endedEvents []outageEventRecord
	for _, e := range o.Events {
		_, ended := o.Ended[e.Event.IncidentID]
		if !ended || keep[e.Event.ID] {
			events = append(events, e)
		} else if !e.Event.OccurredAt.Before(now.Add(-30*24*time.Hour)) || e.KeepUntilProjected && r.needsOutageProjectionLocked(e) {
			endedEvents = append(endedEvents, e)
		}
	}
	if len(endedEvents) > 1000 {
		// 先给刚恢复的长故障一次站内投影机会，仍使用既有结束事件预算。
		sort.SliceStable(endedEvents, func(i, j int) bool {
			left := endedEvents[i].KeepUntilProjected && r.needsOutageProjectionLocked(endedEvents[i])
			right := endedEvents[j].KeepUntilProjected && r.needsOutageProjectionLocked(endedEvents[j])
			if left != right {
				return !left
			}
			return o.Ended[endedEvents[i].Event.IncidentID].Before(o.Ended[endedEvents[j].Event.IncidentID])
		})
		endedEvents = endedEvents[len(endedEvents)-1000:]
	}
	o.Events = append(events, endedEvents...)
	used := map[string]bool{}
	for _, e := range o.Events {
		used[e.Event.IncidentID] = true
	}
	for id := range o.Ended {
		if !used[id] {
			delete(o.Ended, id)
		}
	}
	for id, starts := range o.Starts {
		kept := starts[:0]
		for _, stamp := range starts {
			if stamp.After(now.Add(-time.Minute)) {
				kept = append(kept, stamp)
			}
		}
		if len(kept) == 0 {
			delete(o.Starts, id)
		} else {
			o.Starts[id] = kept
		}
	}
	for key, floor := range o.Floors {
		if !now.Before(floor.Window.Add(time.Minute)) {
			delete(o.Floors, key)
		}
	}
}

var outageBackoffs = [...]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// ReserveNotificationRate 与独立故障 outbox 共用每通道的持久发送窗口。
func (dbs *Databases) ReserveNotificationRate(ctx context.Context, channel string, now time.Time) (allowed bool, next time.Time, configured bool, err error) {
	if dbs == nil || dbs.replication == nil {
		return false, time.Time{}, false, nil
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	old := copyOutage(r.record.Outage)
	allowed, next = r.record.Outage.reserveRate(channel, now)
	if !allowed {
		return false, next, true, nil
	}
	if err = r.persistLocked(ctx); err != nil {
		r.record.Outage = old
		return false, time.Time{}, true, err
	}
	return true, time.Time{}, true, nil
}
func (o *outageControl) reserveRate(channel string, now time.Time) (bool, time.Time) {
	if o.Starts == nil {
		o.Starts = map[string][]time.Time{}
	}
	starts := o.Starts[channel]
	kept := starts[:0]
	var first time.Time
	for _, stamp := range starts {
		if stamp.After(now.Add(-time.Minute)) {
			kept = append(kept, stamp)
			if first.IsZero() || stamp.Before(first) {
				first = stamp
			}
		}
	}
	o.Starts[channel] = kept
	if len(kept) >= 20 {
		return false, first.Add(time.Minute)
	}
	o.Starts[channel] = append(kept, now)
	return true, time.Time{}
}
