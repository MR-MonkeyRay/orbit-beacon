package nodes_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"reflect"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
	"orbitbeacon/internal/timekey"
)

func TestMySQLNodePublicPaginationAndProbeBytes(t *testing.T) {

	f := storagetest.Start(t)
	db := f.Open(t, storage.MainDomain)
	raw := f.Raw(t, storage.MainDomain)
	now := time.Date(2026, 11, 1, 2, 3, 4, 987654321, time.UTC)
	s := nodes.NewService(db, nodes.Options{Now: func() time.Time { return now }})
	ctx := context.Background()
	var ids []string
	for i, settings := range []string{`not json`, `{"enabled":true,"name":"   "}`, `{"enabled":true,"name":"公开一"}`, `{"enabled":true,"name":"公开二"}`, `{"enabled":true,"name":"公开三"}`} {
		n, err := s.CreatePending(ctx, nodes.CreateRequest{Name: fmt.Sprintf("private-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		credential := sha256.Sum256([]byte(n.ID))
		if _, err := raw.Exec(`UPDATE nodes SET status='unknown',token_hash=?,first_enrolled_at=?,last_seen=?,created_at=?,public_settings=?,run_generation='18446744073709551615',report_session_id='session' WHERE id=?`, credential[:], timekey.Format(now), timekey.Format(now), timekey.Format(now.Add(time.Duration(i)*time.Second)), settings, n.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
	}
	tags := []string{"a ", "A", "a"}
	got, err := s.PatchMetadata(ctx, ids[2], nodes.MetadataPatch{Tags: &tags})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Tags, []string{"A", "a", "a "}) {
		t.Fatalf("tags=%q", got.Tags)
	}
	quota := "18446744073709551615"
	if settings, err := s.PatchTrafficSettings(ctx, ids[2], nodes.TrafficSettingsPatch{QuotaBytes: &quota}); err != nil || settings.QuotaBytes != quota {
		t.Fatalf("quota=%+v err=%v", settings, err)
	}
	if _, err := raw.Exec(`INSERT INTO node_availability_daily(node_id,day,online_seconds,offline_seconds) VALUES(?,?,60,30)`, ids[3], now.Format(time.DateOnly)); err != nil {
		t.Fatal(err)
	}
	page, more, err := s.ListPublicRecords(ctx, 1, 1, now)
	if err != nil || !more || len(page) != 1 || page[0].ID != ids[3] || page[0].Generation != ^uint64(0) || !page[0].LastSeen.Equal(now) {
		t.Fatalf("page=%+v more=%v err=%v", page, more, err)
	}
	if !bytes.Contains([]byte(page[0].AvailabilityJSON), []byte(`"online_seconds":60`)) {
		t.Fatal(page[0].AvailabilityJSON)
	}
	for _, target := range []string{"https://example.com/" + string(bytes.Repeat([]byte("a"), 1004)), "https://example.com/" + string(bytes.Repeat([]byte("é"), 502))} {
		settings := v1.DefaultRuntimeSettings()
		settings.ProbeTargets = []string{target}
		runtime, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`UPDATE nodes SET agent_runtime=? WHERE id=?`, string(runtime), ids[2]); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`INSERT INTO probe_latest(node_id,target,ok,error,checked_at) VALUES(?,?,1,'',?)`, ids[2], target, timekey.Format(now)); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms,latency_max_ms) VALUES(?,?,?,1,0,2.5,2.5)`, ids[2], target, timekey.Format(now.Truncate(time.Hour))); err != nil {
			t.Fatal(err)
		}
		probes, err := s.ObservedProbes(ctx, ids[2], now.Add(-24*time.Hour))
		if err != nil || len(probes) != 1 || probes[0].Target != target || probes[0].Checks24H != 1 {
			t.Fatalf("probes=%+v err=%v", probes, err)
		}
		points, err := s.ProbeHistory(ctx, ids[2], target, now.Add(-24*time.Hour))
		if err != nil || len(points) != 1 || points[0].Checks != 1 {
			t.Fatalf("points=%+v err=%v", points, err)
		}
	}
	if _, err := s.LiveRecords(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LiveDetail(ctx, ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CleanupProbeHistory(ctx, now.Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
}
