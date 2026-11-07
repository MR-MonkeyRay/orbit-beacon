package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"orbitbeacon/internal/server/storage"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func publicPageFixture(t *testing.T, count int) *coalesceFixture {
	t.Helper()
	f := newCoalesceFixture(t, time.Second)
	if _, err := f.db.Exec("UPDATE nodes SET created_at=? WHERE id=?", timekey.Format(f.now.Add(-time.Second)), f.id); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < count; i++ {
		node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: fmt.Sprint("private-", i)})
		if err != nil {
			t.Fatal(err)
		}
		credential := fmt.Sprintf("%032d", i)
		if _, err := f.db.Exec(`UPDATE nodes SET status='unknown',token_hash=?,first_enrolled_at=?,last_seen=?,run_generation='1',report_session_id='session',public_settings='{"enabled":true,"name":"Public"}' WHERE id=?`, []byte(credential), timekey.Format(f.now), timekey.Format(f.now), node.ID); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestPublicPageLookaheadAndProjectionDoNotCreateState(t *testing.T) {
	for _, limit := range []int{1, 100} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := publicPageFixture(t, limit+1)
			page, err := f.service.PublicPage(context.Background(), f.nodes, limit, 0, time.Second, func() time.Time { return f.now })
			if err != nil || len(page.Nodes) != limit || !page.HasMore || page.NextOffset == nil || *page.NextOffset != limit {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			if len(f.service.states) != 1 {
				t.Fatalf("public projection created ingest states: %d", len(f.service.states))
			}
		})
	}
}

func TestPublicPageCoalescesMissesAndAllowsWaiterCancellation(t *testing.T) {
	f := publicPageFixture(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state := f.service.acquireExistingState(f.id)
	defer f.service.releaseState(state)
	state.mu.Lock()
	var unlock sync.Once
	defer unlock.Do(state.mu.Unlock)
	results := make(chan error, 20)
	for range 20 {
		go func() {
			page, err := f.service.PublicPage(ctx, f.nodes, 1, 0, time.Second, func() time.Time { return f.now })
			if err == nil && len(page.Nodes) != 1 {
				err = errors.New("incomplete page")
			}
			results <- err
		}()
	}
	for {
		f.service.mu.Lock()
		refs := state.refs
		f.service.mu.Unlock()
		if refs == 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("page did not reach projection")
		default:
			runtime.Gosched()
		}
	}
	waitCtx, cancelWait := context.WithCancel(ctx)
	waitResult := make(chan error, 1)
	go func() {
		_, err := f.service.PublicPage(waitCtx, f.nodes, 1, 0, time.Second, func() time.Time { return f.now })
		waitResult <- err
	}()
	cancelWait()
	if err := <-waitResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation: %v", err)
	}
	unlock.Do(state.mu.Unlock)
	for range 20 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(f.service.publicPages.entries) != 1 || len(f.service.publicPages.calls) != 0 {
		t.Fatal("coalesced call did not publish one cache entry")
	}
}

func TestProjectPublicRecordChecksEveryFenceWithoutCreatingState(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{})
	record := nodes.PublicRecord{ID: "public", Settings: nodes.PublicSettings{Name: "Public"}, LastSeen: now, Generation: 1, Session: "session"}
	out, _ := s.ProjectPublicRecord(record, now)
	if out.Status != "online" || out.CPUPercent != nil || len(s.states) != 0 {
		t.Fatalf("cold projection: %+v states=%d", out, len(s.states))
	}
	state := s.acquireState(record.ID)
	state.generation, state.session = record.Generation, record.Session
	state.public = publicMetrics{receivedAt: now, cpu: publicMetric{valid: true, value: 25, intervalMS: 10000}}
	s.releaseState(state)
	out, _ = s.ProjectPublicRecord(record, now)
	if out.CPUPercent == nil || *out.CPUPercent != 25 {
		t.Fatalf("matching fence: %+v", out)
	}
	for _, fence := range []string{"credential", "credential_epoch", "generation", "session", "received_at"} {
		t.Run(fence, func(t *testing.T) {
			changed := record
			switch fence {
			case "credential":
				changed.Credential[0]++
			case "credential_epoch":
				changed.CredentialEpoch++
			case "generation":
				changed.Generation++
			case "session":
				changed.Session += "-new"
			case "received_at":
				changed.LastSeen = changed.LastSeen.Add(-time.Nanosecond)
			}
			out, _ := s.ProjectPublicRecord(changed, now)
			if out.Status != "online" || out.CPUPercent != nil {
				t.Fatalf("mismatched %s: %+v", fence, out)
			}
		})
	}
}

func TestPublicPageQueriesBeforeTakingStateLock(t *testing.T) {
	f := publicPageFixture(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state := f.service.acquireExistingState(f.id)
	f.service.releaseState(state)
	state.mu.Lock()
	var unlock sync.Once
	defer unlock.Do(state.mu.Unlock)
	if _, err := f.db.Exec("UPDATE nodes SET public_settings='{}' WHERE id=?", f.id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.service.PublicPage(ctx, f.nodes, 1, 0, time.Second, func() time.Time { return f.now })
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		unlock.Do(state.mu.Unlock)
		<-done
		t.Fatal("database query waited for ingest state mutex")
	}
}

func TestPublicPageCacheUsesEffectiveWebIntervalAndStaysBounded(t *testing.T) {
	for _, seconds := range []int{2, 10, 30, 60} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			f := publicPageFixture(t, 2)
			ttl := time.Duration(seconds) * time.Second
			clock := func() time.Time { return f.now }
			first, err := f.service.PublicPage(context.Background(), f.nodes, 1, 0, ttl, clock)
			if err != nil {
				t.Fatal(err)
			}
			f.now = f.now.Add(ttl - time.Nanosecond)
			cached, err := f.service.PublicPage(context.Background(), f.nodes, 1, 0, ttl, clock)
			if err != nil || !cached.GeneratedAt.Equal(first.GeneratedAt) {
				t.Fatalf("cache expired before %v: %v", ttl, err)
			}
			f.now = f.now.Add(time.Nanosecond)
			expired, err := f.service.PublicPage(context.Background(), f.nodes, 1, 0, ttl, clock)
			if err != nil || !expired.GeneratedAt.Equal(f.now) {
				t.Fatalf("cache did not expire at %v: %v", ttl, err)
			}
			for offset := 1; offset <= 140; offset++ {
				if _, err := f.service.PublicPage(context.Background(), f.nodes, 1, offset, ttl, clock); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.service.publicPages.entries) > 128 {
				t.Fatalf("unbounded pages: %d", len(f.service.publicPages.entries))
			}
		})
	}
}

func TestPublicAvailabilityRealSQLiteBatchAndCache(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "public.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("private-node-%d", i)
		if _, err := db.Exec(`INSERT INTO nodes(id,name,status,token_hash,first_enrolled_at,created_at,public_settings,run_generation,report_session_id,last_seen)
 VALUES(?,?,'unknown',?,?,?,? ,?,?,?)`, id, "private-name", []byte(strings.Repeat(fmt.Sprint(i), 32)), timekey.Format(now), timekey.Format(now), `{"enabled":true,"name":"Public"}`, fmt.Sprint(i), "session", timekey.Format(now)); err != nil {
			t.Fatal(err)
		}
		// 每个节点的百分比不同，串节点时断言会失败。
		if _, err := db.Exec(`INSERT INTO node_availability_daily VALUES(?,'2026-11-04',?,?)`, id, 86400-(i+1)*8640, (i+1)*8640); err != nil {
			t.Fatal(err)
		}
	}
	ns := nodes.NewService(stableDatabase(t, db), nodes.Options{})
	s := NewService(stableDatabase(t, db), Options{})
	for range 2 {
		page, err := s.PublicPage(ctx, ns, 100, 0, time.Minute, func() time.Time { return now })
		if err != nil || len(page.Nodes) != 3 {
			t.Fatalf("page %v %v", page, err)
		}
		for i, n := range page.Nodes {
			want := float64(90 - i*10)
			if n.Availability.StartDay != "2026-10-07" || len(n.Availability.Days) != 30 || *n.Availability.Percent != want || *n.Availability.Days[28] != want || n.Availability.Days[29] != nil {
				t.Fatalf("projection %+v", n.Availability)
			}
		}
		if page.Nodes[0].Status != "unknown" {
			t.Fatal("identity not ready should be unknown")
		}
		raw, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private-node", "private-name", "online_seconds", "offline_seconds", "node_id"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("leaked %s", raw)
			}
		}
	}
}
