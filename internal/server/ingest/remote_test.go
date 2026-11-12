package ingest

import (
	"context"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
	"testing"
	"time"
)

func TestRemoteObservationStableIPAndRegionRequalification(t *testing.T) {
	ctx := context.Background()
	f := newCoalesceFixture(t, 0)
	if _, err := f.db.Exec(`CREATE TABLE remote_writes(n INTEGER NOT NULL); INSERT INTO remote_writes VALUES(0); CREATE TRIGGER count_remote_writes AFTER UPDATE OF remote_ip ON nodes BEGIN UPDATE remote_writes SET n=n+1; END`); err != nil {
		t.Fatal(err)
	}
	eligible, changed, err := f.service.ObserveRemote(ctx, f.id, "::ffff:8.8.8.8")
	if err != nil || !eligible || !changed {
		t.Fatalf("first %v %v %v", eligible, changed, err)
	}
	for i := 0; i < 5; i++ {
		eligible, changed, err = f.service.ObserveRemote(ctx, f.id, "8.8.8.8")
		if err != nil || !eligible || changed {
			t.Fatalf("stable %v %v %v", eligible, changed, err)
		}
	}
	var writes int
	if err = f.db.QueryRow("SELECT n FROM remote_writes").Scan(&writes); err != nil || writes != 1 {
		t.Fatalf("writes %d %v", writes, err)
	}
	if _, err = f.db.Exec("UPDATE nodes SET geo_country='US',geo_location='California',geo_checked_at=? WHERE id=?", timekey.Format(f.now), f.id); err != nil {
		t.Fatal(err)
	}
	report := func() {
		f.now = f.now.Add(time.Second)
		f.report.Seq++
		if _, err := acceptReport(ctx, f.service, coalesceToken, f.report); err != nil {
			t.Fatal(err)
		}
	}
	report()
	eligible, changed, err = f.service.ObserveRemote(ctx, f.id, "8.8.8.8")
	if err != nil || eligible || changed {
		t.Fatalf("fresh %v %v %v", eligible, changed, err)
	}
	region := "Manual"
	if _, err = f.nodes.PatchMetadata(ctx, f.id, nodes.MetadataPatch{Region: &region}); err != nil {
		t.Fatal(err)
	}
	report()
	eligible, _, err = f.service.ObserveRemote(ctx, f.id, "8.8.8.8")
	if err != nil || eligible {
		t.Fatalf("manual %v %v", eligible, err)
	}
	region = ""
	if _, err = f.nodes.PatchMetadata(ctx, f.id, nodes.MetadataPatch{Region: &region}); err != nil {
		t.Fatal(err)
	}
	report()
	eligible, changed, err = f.service.ObserveRemote(ctx, f.id, "8.8.8.8")
	if err != nil || !eligible || changed {
		t.Fatalf("clear region %v %v %v", eligible, changed, err)
	}
	eligible, changed, err = f.service.ObserveRemote(ctx, f.id, "1.1.1.1")
	if err != nil || !eligible || !changed {
		t.Fatalf("new IP %v %v %v", eligible, changed, err)
	}
	n, err := f.nodes.Get(ctx, f.id)
	if err != nil || n.RemoteIP != "1.1.1.1" || n.GeoCountry != "" || n.GeoLocation != "" {
		t.Fatalf("old geo survived %+v %v", n, err)
	}
}
