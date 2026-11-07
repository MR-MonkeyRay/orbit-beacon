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
