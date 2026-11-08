package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

const storageNotificationsKey = "storage_notifications"

type storageChannelSnapshot struct {
	Kind   string `json:"kind"`
	Config Config `json:"config"`
	Secret string `json:"secret"`
}

func defaultStoragePreferences() storage.StorageNotificationPreferences {
	return storage.StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{}}
}

func readStoragePreferences(ctx context.Context, tx *storage.Tx) (storage.StorageNotificationPreferences, error) {
	p := defaultStoragePreferences()
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE `key`=?", storageNotificationsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || len(fields) != 2 || fields["inbox"] == nil || fields["channel_ids"] == nil || string(fields["inbox"]) == "null" || json.Unmarshal([]byte(raw), &p) != nil || p.ChannelIDs == nil {
		return p, ErrUnavailable
	}
	return p, validateStoragePreferences(p)
}

func validateStoragePreferences(p storage.StorageNotificationPreferences) error {
	if p.ChannelIDs == nil || len(p.ChannelIDs) > 20 {
		return validation.Invalid("channel_ids", "requires at most 20 channels")
	}
	seen := make(map[string]bool, len(p.ChannelIDs))
	for _, id := range p.ChannelIDs {
		if !uuid.ValidUUID(id) || seen[id] {
			return validation.Invalid("channel_ids", "invalid or duplicate channel")
		}
		seen[id] = true
	}
	return nil
}

func (s *Service) storageWarning(ctx context.Context, code string, err error) {
	if s.logger != nil {
		s.logger.LogAttrs(ctx, slog.LevelWarn, code, logging.ErrorAttrs(err, "storage_error")...)
	}
}

func (s *Service) StorageNotificationSettings(ctx context.Context) (storage.StorageNotificationSettings, error) {
	lease, err := s.db.Acquire(ctx, storage.Reader)
	if err != nil {
		return storage.StorageNotificationSettings{}, err
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginRead(ctx)
	if err != nil {
		return storage.StorageNotificationSettings{}, err
	}
	defer tx.Rollback()
	p, err := readStoragePreferences(ctx, tx)
	if err != nil {
		return storage.StorageNotificationSettings{}, err
	}
	if err = tx.Commit(); err != nil {
		return storage.StorageNotificationSettings{}, err
	}
	result := storage.StorageNotificationSettings{StorageNotificationPreferences: p}
	if s.opts.Storage != nil {
		snapshot, e := s.opts.Storage.OutageSnapshot(ctx)
		if e != nil {
			s.storageWarning(ctx, "storage_notification_snapshot_failed", e)
		} else {
			result.SnapshotReady = snapshot.Ready
		}
	}
	return result, nil
}

func (s *Service) pauseStorageSnapshot(ctx context.Context, lease *storage.Lease) (storage.OutageSnapshotToken, error) {
	if s.opts.Storage == nil {
		return storage.OutageSnapshotToken{}, nil
	}
	return s.opts.Storage.PauseOutageSnapshot(ctx, lease.Generation())
}

// controlFailure 区分控制持久化失败与已暂停的业务快照读取失败。
func (s *Service) seedStorageRates(ctx context.Context, tx *storage.Tx) (controlFailure bool, err error) {
	if s.opts.Storage == nil {
		return false, nil
	}
	now := s.opts.Now().UTC()
	rows, err := tx.QueryContext(ctx, `SELECT r.channel_id,r.window_start,r.count FROM notification_rate_limits r JOIN notification_channels c ON c.id=r.channel_id ORDER BY r.channel_id`)
	if err != nil {
		return false, err
	}
	type rate struct {
		id     string
		window time.Time
		count  int
	}
	rates := make([]rate, 0, 20)
	for rows.Next() {
		var r rate
		var stamp string
		if err = rows.Scan(&r.id, &stamp, &r.count); err != nil {
			break
		}
		r.window, err = timekey.Parse(stamp)
		if err != nil {
			break
		}
		rates = append(rates, r)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return false, err
	}
	for _, r := range rates {
		if r.count < 0 || r.count > 20 || r.count > 0 && (r.window.IsZero() || r.window.After(now)) {
			return false, ErrUnavailable
		}
	}
	for _, r := range rates {
		if _, err = s.opts.Storage.SeedNotificationRate(ctx, r.id, r.window, r.count, now); err != nil {
			return true, err
		}
	}
	return false, nil
}

// 调用方已持有 SQL guard；revision 绝不能早于 guard 分配。
func (s *Service) finishStorageMutation(ctx context.Context, lease *storage.Lease, tx *storage.Tx, token storage.OutageSnapshotToken, outcome error) {
	rollback := tx.Rollback()
	if s.opts.Storage == nil || token.Revision == 0 || errors.Is(outcome, storage.ErrCommitUnknown) || ctx.Err() != nil {
		return
	}
	if rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
		s.storageWarning(ctx, "storage_notification_rollback_failed", rollback)
		return
	}
	if err := s.publishStorageSnapshot(ctx, lease, token); err != nil {
		s.storageWarning(ctx, "storage_notification_snapshot_failed", err)
	}
}

func (s *Service) removeStorageSubscription(ctx context.Context, tx *storage.Tx, id string) error {
	p, err := readStoragePreferences(ctx, tx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(p.ChannelIDs))
	for _, current := range p.ChannelIDs {
		if current != id {
			ids = append(ids, current)
		}
	}
	if len(ids) == len(p.ChannelIDs) {
		return nil
	}
	p.ChannelIDs = ids
	return writeStoragePreferences(ctx, tx, p)
}

func writeStoragePreferences(ctx context.Context, tx *storage.Tx, p storage.StorageNotificationPreferences) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	query := `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`
	if tx.Dialect() == storage.MySQL {
		query = "INSERT INTO settings(`key`,value) VALUES(?,?) ON DUPLICATE KEY UPDATE value=VALUES(value)"
	}
	_, err = tx.ExecContext(ctx, query, storageNotificationsKey, string(raw))
	return err
}

func (s *Service) UpdateStorageNotificationSettings(ctx context.Context, p storage.StorageNotificationPreferences) (storage.StorageNotificationSettings, error) {
	if err := validateStoragePreferences(p); err != nil {
		return storage.StorageNotificationSettings{}, err
	}
	lease, err := s.db.Acquire(ctx, storage.Writer)
	if err != nil {
		return storage.StorageNotificationSettings{}, err
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return storage.StorageNotificationSettings{}, err
	}
	defer tx.Rollback()
	for _, id := range p.ChannelIDs {
		var enabled bool
		if err = tx.QueryRowContext(ctx, "SELECT enabled FROM notification_channels WHERE id=?", id).Scan(&enabled); errors.Is(err, sql.ErrNoRows) || err == nil && !enabled {
			return storage.StorageNotificationSettings{}, validation.Invalid("channel_ids", "requires enabled channels")
		}
		if err != nil {
			return storage.StorageNotificationSettings{}, err
		}
	}
	token, err := s.pauseStorageSnapshot(ctx, lease)
	if err != nil {
		return storage.StorageNotificationSettings{}, err
	}
	if token.Revision != 0 {
		_, err = s.seedStorageRates(ctx, tx)
	}
	if err != nil {
		return storage.StorageNotificationSettings{}, err
	}
	if err == nil {
		err = writeStoragePreferences(ctx, tx, p)
	}
	if err == nil {
		err = audit.Insert(ctx, tx, "storage_notifications.update", "", s.opts.Now())
	}
	if err == nil {
		err = tx.Commit()
	}
	s.finishStorageMutation(ctx, lease, tx, token, err)
	if err != nil {
		return storage.StorageNotificationSettings{}, err
	}
	return s.StorageNotificationSettings(ctx)
}

func (s *Service) SyncStorageSnapshot(ctx context.Context) error {
	if s.opts.Storage == nil {
		return nil
	}
	lease, err := s.db.Acquire(ctx, storage.Writer)
	if err != nil {
		return err
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return err
	}
	token, err := s.pauseStorageSnapshot(ctx, lease)
	if err == nil && token.Revision != 0 {
		controlFailure, seedErr := s.seedStorageRates(ctx, tx)
		if seedErr != nil {
			rollback := tx.Rollback()
			if rollback != nil {
				return rollback
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if controlFailure {
				return seedErr
			}
			s.storageWarning(ctx, "storage_notification_rate_seed_failed", seedErr)
			return nil
		}
	}
	rollback := tx.Rollback()
	if err != nil {
		return err
	}
	if rollback != nil {
		return rollback
	}
	if token.Revision == 0 {
		return nil
	}
	snapshot, readErr := s.readStorageSnapshot(ctx, lease)
	if snapshot.Preferences.ChannelIDs != nil {
		current, persistErr := s.opts.Storage.PublishOutagePreferences(ctx, token, snapshot.Preferences)
		if persistErr != nil {
			return persistErr
		}
		if !current {
			return nil
		}
	}
	if readErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.storageWarning(ctx, "storage_notification_snapshot_failed", readErr)
		return nil
	}
	_, err = s.opts.Storage.PublishOutageSnapshot(ctx, token, snapshot.Preferences, snapshot.Channels)
	return err
}

func (s *Service) publishStorageSnapshot(ctx context.Context, lease *storage.Lease, token storage.OutageSnapshotToken) error {
	snapshot, readErr := s.readStorageSnapshot(ctx, lease)
	if snapshot.Preferences.ChannelIDs != nil {
		current, err := s.opts.Storage.PublishOutagePreferences(ctx, token, snapshot.Preferences)
		if err != nil {
			return err
		}
		if !current {
			return nil
		}
	}
	if readErr != nil {
		return readErr
	}
	_, err := s.opts.Storage.PublishOutageSnapshot(ctx, token, snapshot.Preferences, snapshot.Channels)
	return err
}

func (s *Service) readStorageSnapshot(ctx context.Context, lease *storage.Lease) (storage.OutageSnapshot, error) {
	snapshot := storage.OutageSnapshot{}
	tx, err := lease.BeginRead(ctx)
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback()
	p, err := readStoragePreferences(ctx, tx)
	if err != nil {
		return snapshot, err
	}
	snapshot.Preferences = p
	if s.opts.StorageBox == nil {
		return snapshot, ErrUnavailable
	}
	snapshot.Channels = make([]storage.OutageChannel, 0, len(p.ChannelIDs))
	for _, id := range p.ChannelIDs {
		c, encrypted, urlCipher, e := scanChannel(tx.QueryRowContext(ctx, "SELECT "+channelColumns+" FROM notification_channels WHERE id=?", id))
		if errors.Is(e, ErrNotFound) {
			return snapshot, ErrUnavailable
		}
		if e != nil {
			return snapshot, e
		}
		if !c.Enabled {
			continue
		}
		plain := storageChannelSnapshot{Kind: c.Kind, Config: c.Config}
		if len(encrypted) > 0 {
			value, e := s.openSecret(id, encrypted)
			if e != nil {
				return snapshot, e
			}
			plain.Secret = string(value)
			clear(value)
		}
		if UsesURL(c.Kind) {
			value, e := s.openSecret(id+":url", urlCipher)
			if e != nil {
				return snapshot, e
			}
			plain.Config.URL = string(value)
			clear(value)
		}
		plain.Config.Template = ""
		plain.Config.URLRedacted = ""
		plain.Config.URLSet = false
		if err = s.validate(Channel{Name: c.Name, Kind: c.Kind, Config: plain.Config}, plain.Secret); err != nil {
			return snapshot, ErrUnavailable
		}
		raw, e := json.Marshal(plain)
		if e != nil {
			return snapshot, e
		}
		ciphertext, e := s.opts.StorageBox.Seal(id, raw)
		clear(raw)
		if e != nil {
			return snapshot, e
		}
		snapshot.Channels = append(snapshot.Channels, storage.OutageChannel{ID: id, Ciphertext: ciphertext})
	}
	if err = tx.Commit(); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (s *Service) openStorageChannel(c storage.OutageChannel) (Channel, string, error) {
	if s.opts.StorageBox == nil {
		return Channel{}, "", ErrUnavailable
	}
	raw, err := s.opts.StorageBox.Open(c.ID, c.Ciphertext)
	if err != nil {
		return Channel{}, "", ErrSecretUnreadable
	}
	defer clear(raw)
	var snapshot storageChannelSnapshot
	if json.Unmarshal(raw, &snapshot) != nil {
		return Channel{}, "", ErrSecretUnreadable
	}
	channel := Channel{ID: c.ID, Name: "storage", Kind: snapshot.Kind, Enabled: true, Config: snapshot.Config}
	if s.validate(channel, snapshot.Secret) != nil {
		return Channel{}, "", ErrSecretUnreadable
	}
	return channel, snapshot.Secret, nil
}

// RunStorage 不持有业务 lease 发送，故障主库不会阻塞外部故障通知。
func (s *Service) RunStorage(ctx context.Context) {
	if !s.opts.Storage.ReplicationEnabled() {
		return
	}
	s.mu.Lock()
	if s.storageRunning || s.closed.Load() {
		s.mu.Unlock()
		return
	}
	s.storageRunning = true
	s.storageDone = make(chan struct{})
	done := s.storageDone
	s.mu.Unlock()
	defer close(done)
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.stopCtx, cancel)
	defer stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var generation uint64
	var nextSync time.Time
	for work.Err() == nil {
		now := s.opts.Now()
		if s.db.Ready() && !now.Before(nextSync) {
			snapshot, err := s.opts.Storage.OutageSnapshot(work)
			if err == nil && (generation != s.db.Generation() || !snapshot.Ready) {
				if err = s.SyncStorageSnapshot(work); err == nil {
					generation = s.db.Generation()
				} else {
					s.storageWarning(work, "storage_notification_sync_failed", err)
				}
			}
			nextSync = now.Add(5 * time.Second)
		}
		s.processStorage(work)
		s.projectStorage(work)
		select {
		case <-work.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) processStorage(ctx context.Context) {
	work, err := s.opts.Storage.ClaimOutageDeliveries(ctx, s.opts.Now(), 4)
	if err != nil {
		s.storageWarning(ctx, "storage_notification_claim_failed", err)
		return
	}
	var wg sync.WaitGroup
	for _, delivery := range work {
		wg.Add(1)
		go func(d storage.OutageDelivery) {
			defer wg.Done()
			channel, secret, err := s.openStorageChannel(d.Channel)
			if err != nil {
				_, pauseErr := s.opts.Storage.PauseOutageSnapshot(ctx, d.Token.Generation)
				if pauseErr != nil {
					s.storageWarning(ctx, "storage_notification_pause_failed", pauseErr)
				}
				s.warnSecretUnreadable(d.Channel.ID)
				return
			}
			event := d.Event
			var role *string
			if event.ActiveRole != "" {
				value := event.ActiveRole
				role = &value
			}
			result := s.sender.sendStorage(ctx, channel, secret, StoragePayload{Kind: "storage", EventID: event.ID, Domain: string(event.Domain), Transition: event.Transition, ActiveRole: role, ReasonCode: event.ReasonCode, OccurredAt: event.OccurredAt}, func(sendCtx context.Context) (bool, error) {
				return s.opts.Storage.OutageDeliveryCurrent(sendCtx, d.ID, d.Token)
			})
			if ctx.Err() != nil {
				return
			}
			err = s.opts.Storage.FinishOutageDelivery(ctx, d.ID, storage.OutageResult{OK: result.OK, ErrorCode: result.ErrorCode, Retry: result.retry, RetryAfter: result.retryAfter}, s.opts.Now())
			if err != nil {
				s.storageWarning(ctx, "storage_notification_result_failed", err)
			}
		}(delivery)
	}
	wg.Wait()
}

func (s *Service) projectStorage(ctx context.Context) {
	lease, err := s.db.Acquire(ctx, storage.Writer)
	if err != nil {
		return
	}
	defer lease.Close()
	ctx = lease.Context()
	events, err := s.opts.Storage.OutageProjection(ctx, lease.Generation(), 64)
	if err != nil || len(events) == 0 {
		return
	}
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback()
	preferences, err := readStoragePreferences(ctx, tx)
	if err != nil || !preferences.Inbox {
		return
	}
	for _, event := range events {
		var role *string
		if event.ActiveRole != "" {
			value := event.ActiveRole
			role = &value
		}
		details, err := json.Marshal(struct {
			Domain     storage.Domain `json:"domain"`
			Transition string         `json:"transition"`
			ActiveRole *string        `json:"active_role"`
			ReasonCode string         `json:"reason_code"`
			OccurredAt time.Time      `json:"occurred_at"`
		}{event.Domain, event.Transition, role, event.ReasonCode, event.OccurredAt})
		if err != nil {
			return
		}
		if err = inbox.Insert(ctx, tx, inbox.Event{Kind: "storage", SourceID: event.ID, Transition: event.Transition, Severity: storageSeverity(event.Transition), CreatedAt: s.opts.Now(), Details: details}); err != nil {
			return
		}
	}
	if err = tx.Commit(); err != nil {
		return
	}
	for _, event := range events {
		if err = s.opts.Storage.MarkOutageProjected(ctx, event.ID, lease.Generation()); err != nil {
			return
		}
	}
}
