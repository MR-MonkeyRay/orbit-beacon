package ingest

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func TestPublicPageCapturesVisibilityVersionBeforeRacingMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	ns := nodes.NewService(db, nodes.Options{Now: func() time.Time { return now }})
	node, err := ns.CreatePending(ctx, nodes.CreateRequest{Name: "private"})
	if err != nil {
		t.Fatal(err)
	}
	credential := sha256.Sum256([]byte("public-page-race-credential"))
	if _, err := db.ExecContext(ctx, `UPDATE nodes SET status='unknown', token_hash=?, first_enrolled_at=?, last_seen=?, run_generation='1', report_session_id='session', public_settings='{"enabled":true,"name":"Published"}' WHERE id=?`, credential[:], timekey.Format(now), timekey.Format(now), node.ID); err != nil {
		t.Fatal(err)
	}

	s := NewService(db, Options{Now: func() time.Time { return now }})
	state := s.acquireState(node.ID)
	state.credential = credential
	state.generation = 1
	state.session = "session"
	state.public.receivedAt = now
	s.releaseState(state)
	state.mu.Lock()
	locked := true
	defer func() {
		if locked {
			state.mu.Unlock()
		}
	}()

	result := make(chan struct {
		page PublicPage
		err  error
	}, 1)
	clock := func() time.Time { return now }
	go func() {
		page, err := s.PublicPage(ctx, ns, 100, 0, clock)
		result <- struct {
			page PublicPage
			err  error
		}{page: page, err: err}
	}()

	// refs becomes one only after the page query returned its record and
	// projection acquired the existing state before blocking on state.mu.
	for {
		s.mu.Lock()
		refs := state.refs
		s.mu.Unlock()
		if refs == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("page projection did not reach the state barrier")
		default:
			runtime.Gosched()
		}
	}
	if got := ns.PublicVisibilityVersion(); got != 0 {
		t.Fatalf("version before mutation = %d, want 0", got)
	}
	disabled := false
	if _, err := ns.PatchPublicSettings(ctx, node.ID, nodes.PublicSettingsPatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if got := ns.PublicVisibilityVersion(); got != 1 {
		t.Fatalf("version after unpublish = %d, want 1", got)
	}
	state.mu.Unlock()
	locked = false

	first := <-result
	if first.err != nil || len(first.page.Nodes) != 1 || first.page.Nodes[0].Name != "Published" {
		t.Fatalf("in-flight page = %+v, err=%v", first.page, first.err)
	}
	key := publicPageKey{service: ns, limit: 100, offset: 0}
	s.publicPages.mu.Lock()
	entry, ok := s.publicPages.entries[key]
	s.publicPages.mu.Unlock()
	if !ok || entry.version != 0 {
		t.Fatalf("raced cache entry exists=%v version=%d, want captured version 0", ok, entry.version)
	}

	second, err := s.PublicPage(ctx, ns, 100, 0, clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Nodes) != 0 {
		t.Fatalf("request after unpublish reused stale page: %+v", second)
	}
	s.publicPages.mu.Lock()
	entry = s.publicPages.entries[key]
	s.publicPages.mu.Unlock()
	if entry.version != 1 {
		t.Fatalf("refreshed cache version = %d, want 1", entry.version)
	}
}
