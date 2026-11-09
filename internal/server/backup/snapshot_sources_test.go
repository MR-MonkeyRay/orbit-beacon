package backup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSnapshotUsesSelectedDatabaseInsteadOfDataDirectory(t *testing.T) {
	selected := restoreFixture(t, "selected")
	other := restoreFixture(t, "other")
	work := filepath.Join(framingTempDir(t), "payloads")
	if _, err := CreateSnapshotPayloads(context.Background(), snapshotSources(t, selected), other, work, []Category{Events}, time.Now); err != nil {
		t.Fatal(err)
	}
	payload, err := openPayload(t.Context(), filepath.Join(work, "events.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer payload.Close()
	var details string
	if err := payload.QueryRow("SELECT details FROM audit_events WHERE action='fixture'").Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, "selected") || strings.Contains(details, "other") {
		t.Fatalf("snapshot read the data-directory database: %s", details)
	}
}

func TestMonitoringRejectsMissingActiveHistoryDespiteLocalFile(t *testing.T) {
	dir := restoreFixture(t, "remote-history")
	sources := snapshotSources(t, dir)
	sources.History.Close()
	sources.History = nil
	_, err := CreateSnapshotPayloads(t.Context(), sources, dir, filepath.Join(framingTempDir(t), "payloads"), []Category{Monitoring}, time.Now)
	var safe *Error
	if !errors.As(err, &safe) || safe.Code != "history_unavailable" {
		t.Fatalf("missing active history read a local fallback: %v", err)
	}
}
