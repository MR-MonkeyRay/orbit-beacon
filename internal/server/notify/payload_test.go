package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"orbitbeacon/internal/timekey"
)

func TestSummaryFullPayloadConflictRollsBack(t *testing.T) {
	f := newFixture(t)
	for i := range MaxPendingPerChannel {
		f.enqueue(t, f.note(fmt.Sprintf("event-%d", i)))
	}
	note := f.note("collision")
	payload := notificationPayload(note)
	payload.SummaryCount = 2
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO notification_deliveries(id,channel_id,event_id,state,next_attempt_at,payload_json,created_at,updated_at) VALUES('collision',?,?,'sent',?,?,?,?)`, f.channel.ID, note.EventID, timekey.Format(f.now), string(raw), timekey.Format(f.now), timekey.Format(f.now)); err != nil {
		t.Fatal(err)
	}
	tx, err := beginMainWrite(t, f.db, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Enqueue(context.Background(), tx, note); err == nil {
		tx.Rollback()
		t.Fatal("summary collision accepted")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	f.count(t, `SELECT sum(coalesced_count) FROM notification_deliveries WHERE state='pending'`, MaxPendingPerChannel)
	f.count(t, `SELECT count(*) FROM notification_deliveries WHERE event_id='collision'`, 1)
}
