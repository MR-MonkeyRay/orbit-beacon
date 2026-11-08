package backup

import (
	"context"
	"testing"
)

func TestArchiveStorageNotificationSettingsReferences(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{`{"inbox":true,"channel_ids":["99999999-9999-4999-8999-999999999999"]}`, true},
		{`{"inbox":false,"channel_ids":[]}`, true},
		{`{"inbox":true,"channel_ids":["aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"]}`, false},
		{`{"inbox":true,"channel_ids":["99999999-9999-4999-8999-999999999999","99999999-9999-4999-8999-999999999999"]}`, false},
		{`{"channel_ids":[]}`, false},
		{`{"inbox":true,"channel_ids":null}`, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			dir := restoreFixture(t, "source")
			db := writableRestoreFixture(t, dir)
			if _, err := db.Exec(`INSERT INTO settings(key,value) VALUES('storage_notifications',?)`, tc.value); err != nil {
				t.Fatal(err)
			}
			db.Close()
			sources := snapshotSources(t, dir)
			_, err := CreateSnapshotPayloads(context.Background(), sources, dir, framingTempDir(t), []Category{Configuration}, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}

func TestArchiveStorageInboxMetadata(t *testing.T) {
	row := payloadRow{"id": validationID, "kind": "storage", "source_id": validationID, "transition": "manual_required", "details_json": `{"domain":"main","transition":"manual_required","active_role":null,"reason_code":"recovery_timeout","occurred_at":"2026-11-07T12:00:00Z"}`}
	if err := validatePayloadRow("inbox_notifications", row, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	row["details_json"] = `{"domain":"other","transition":"manual_required","active_role":null,"reason_code":"recovery_timeout","occurred_at":"2026-11-07T12:00:00Z"}`
	if err := validatePayloadRow("inbox_notifications", row, nil, nil, nil); err == nil {
		t.Fatal("invalid storage details accepted")
	}
}

func TestArchiveStoragePreferencesAllowDisabledExistingChannel(t *testing.T) {
	dir := restoreFixture(t, "source")
	db := writableRestoreFixture(t, dir)
	if _, err := db.Exec(`UPDATE notification_channels SET enabled=0 WHERE id='99999999-9999-4999-8999-999999999999'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settings(key,value) VALUES('storage_notifications','{"inbox":true,"channel_ids":["99999999-9999-4999-8999-999999999999"]}')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err := CreateSnapshotPayloads(context.Background(), snapshotSources(t, dir), dir, framingTempDir(t), []Category{Configuration}, nil)
	if err != nil {
		t.Fatal(err)
	}
}
