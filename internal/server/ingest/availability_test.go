package ingest

import (
	"testing"
	"time"

	"orbitbeacon/internal/server/availability"
	"orbitbeacon/internal/server/nodes"
)

func TestAvailabilityUsesLatestAcceptedReport(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(map[bool]string{false: "current identity", true: "replaced identity"}[replaced], func(t *testing.T) {
			f := newCoalesceFixture(t, 30*time.Second)
			started := f.now
			f.now = started.Add(10 * time.Second)
			accounting := availability.NewService(f.nodes.Database(), func() time.Time { return f.now }, nil, f.service.LiveReceivedAt)
			if err := accounting.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.now = started.Add(20 * time.Second)
			f.report.Seq++
			f.report.SampledAt = f.now
			if _, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err != nil {
				t.Fatal(err)
			}
			if got := f.writes(t); got != 1 {
				t.Fatalf("second accepted report should remain coalesced, writes=%d", got)
			}
			if replaced {
				if _, err := f.db.Exec("UPDATE nodes SET credential_epoch=credential_epoch+1 WHERE id=?", f.id); err != nil {
					t.Fatal(err)
				}
			}
			for _, seconds := range []int{70, 130, 190} {
				f.now = started.Add(time.Duration(seconds) * time.Second)
				if err := accounting.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			var online, offline int64
			if err := f.db.QueryRow("SELECT online_seconds,offline_seconds FROM node_availability_daily WHERE node_id=?", f.id).Scan(&online, &offline); err != nil {
				t.Fatal(err)
			}
			wantOnline, wantOffline := int64(180), int64(0)
			if replaced {
				wantOnline, wantOffline = 120, 60
			} else {
				latest, err := f.service.Latest(t.Context(), f.id)
				if err != nil {
					t.Fatal(err)
				}
				if got := nodes.Presence("unknown", true, &latest.ReceivedAt, f.now); got != "online" {
					t.Fatalf("last accepted report age=%s, presence=%s", f.now.Sub(latest.ReceivedAt), got)
				}
			}
			if online != wantOnline || offline != wantOffline {
				t.Fatalf("availability=%ds online/%ds offline, want %ds/%ds", online, offline, wantOnline, wantOffline)
			}
		})
	}
}
