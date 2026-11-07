package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func TestPublicProjectionConcurrentReportsRotationAndDeletion(t *testing.T) {
	f := newAgentHTTP(t, 2)
	id, token, _ := enrollHTTP(t, f, "private-concurrent", 80)
	enabled, name := true, "Public concurrent"
	if _, err := f.nodes.PatchPublicSettings(context.Background(), id, nodes.PublicSettingsPatch{Enabled: &enabled, Name: &name}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for range 50 {
			out, err := publicNode(context.Background(), f.nodes, f.ingest, id, f.now)
			if err != nil && !errors.Is(err, nodes.ErrNotFound) {
				t.Errorf("concurrent public read: %v", err)
			}
			if out.Status != "online" && (out.CPUPercent != nil || out.MemoryPercent != nil) {
				t.Errorf("non-online projection exposed metrics: %+v", out)
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for seq := uint64(1); seq <= 10; seq++ {
			var report v1.Report
			if err := json.Unmarshal([]byte(reportBody(1, seq, "race-session", 20)), &report); err != nil {
				panic(err)
			}
			_, _ = acceptReport(context.Background(), f.ingest, token, report)
		}
	}()
	close(start)
	if _, err := f.db.Exec(`UPDATE nodes SET token_hash=randomblob(32),run_generation='0',report_session_id='' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := f.nodes.Delete(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if _, err := publicNode(context.Background(), f.nodes, f.ingest, id, f.now); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("deleted concurrent node exposed: %v", err)
	}
}
