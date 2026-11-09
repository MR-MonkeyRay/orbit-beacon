package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"testing"
	"time"

	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
	"orbitbeacon/internal/timekey"
)

func TestMySQLNotificationPayloadIdempotencyAndSummaryConflict(t *testing.T) {

	f := storagetest.Start(t)
	db := f.Open(t, storage.MainDomain)
	raw := f.Raw(t, storage.MainDomain)
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 2, 3, 4, 987654321, time.UTC)
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32), "orbitbeacon notification channel v1")
	if err != nil {
		t.Fatal(err)
	}
	s := New(db, box, Options{Now: func() time.Time { return now }})
	var p Patch
	if err := json.Unmarshal([]byte(`{"name":"webhook","kind":"webhook","enabled":true,"config":{"url":"https://example.com/hook"}}`), &p); err != nil {
		t.Fatal(err)
	}
	channel, err := s.Save(ctx, "", p)
	if err != nil {
		t.Fatal(err)
	}
	var rp alerts.Patch
	if err := json.Unmarshal([]byte(`{"name":"CPU","enabled":true,"scope_kind":"all","metric":"cpu_percent","comparator":">","threshold":80,"for_seconds":0,"severity":"warning","channel_ids":["`+channel.ID+`"]}`), &rp); err != nil {
		t.Fatal(err)
	}
	rule, err := alerts.NewService(db, func() time.Time { return now }).Save(ctx, "", rp)
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(n alerts.Notification) error {
		lease, err := db.Acquire(ctx, storage.Writer)
		if err != nil {
			return err
		}
		defer lease.Close()
		tx, err := lease.BeginWrite(lease.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := s.Enqueue(lease.Context(), tx, n); err != nil {
			return err
		}
		return tx.Commit()
	}
	note := alerts.Notification{EventID: "same-event", Rule: rule, NodeID: "node", NodeName: "Case ", State: "firing", StartedAt: now}
	for range 2 {
		if err := enqueue(note); err != nil {
			t.Fatal(err)
		}
	}
	note.NodeName = "case "
	if err := enqueue(note); err != nil {
		t.Fatal(err)
	}
	note.NodeName = "Case"
	if err := enqueue(note); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := raw.QueryRow(`SELECT count(*) FROM notification_deliveries`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	for i := 3; i < MaxPendingPerChannel; i++ {
		note.EventID = fmt.Sprintf("event-%d", i)
		if err := enqueue(note); err != nil {
			t.Fatal(err)
		}
	}
	// 构造别行持有目标摘要完整字节，合并必须中止，不能吞掉冲突。
	note.EventID = "summary-conflict"
	payload := notificationPayload(note)
	payload.SummaryCount = 2
	encoded, _ := json.Marshal(payload)
	if _, err := raw.Exec(`INSERT INTO notification_deliveries(id,channel_id,event_id,state,next_attempt_at,payload_json,created_at,updated_at) VALUES('collision',?,?,'sent',?,?,?,?)`, channel.ID, note.EventID, timekey.Format(now), string(encoded), timekey.Format(now), timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	if err := enqueue(note); err == nil {
		t.Fatal("summary collision accepted")
	}
	if err := raw.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE state='pending'`).Scan(&count); err != nil || count != MaxPendingPerChannel {
		t.Fatalf("pending=%d err=%v", count, err)
	}
	if _, err := s.Cleanup(ctx, now.Add(31*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	lease, err := db.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if ok, _, err := s.reserveRate(lease.Context(), tx, channel.ID); err != nil || !ok {
		t.Fatalf("rate=%v err=%v", ok, err)
	}
	if ok, _, err := s.reserveRate(lease.Context(), tx, channel.ID); err != nil || !ok {
		t.Fatalf("rate upsert=%v err=%v", ok, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
