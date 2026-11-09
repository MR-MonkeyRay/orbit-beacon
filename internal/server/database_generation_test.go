package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestDatabaseSwitchPausesDrainsAndReopensStreams(t *testing.T) {
	f := newConsoleLiveFixture(t)
	for range 2 {
		stream, ok := f.h.streams.acquire("192.0.2.1")
		if !ok {
			t.Fatal("stream admission remained closed")
		}
		released := make(chan struct{})
		go func() { <-stream.ctx.Done(); stream.release(); close(released) }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := f.h.PrepareDatabaseSwitch(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		<-released
		if extra, ok := f.h.streams.acquire("192.0.2.2"); ok {
			extra.release()
			t.Fatal("stream entered during unpublished generation")
		}
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes", nil))
		if w.Code != 503 {
			t.Fatalf("unpublished public projection status=%d", w.Code)
		}
		lease, err := f.h.nodes.Database().Acquire(context.Background(), storage.Reader)
		if err != nil {
			t.Fatal(err)
		}
		err = f.h.PublishDatabaseGeneration(lease.Context(), lease)
		lease.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	f.h.CloseLiveStreams()
	lease, err := f.h.nodes.Database().Acquire(context.Background(), storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err = f.h.PublishDatabaseGeneration(lease.Context(), lease); err != nil {
		t.Fatal(err)
	}
	if stream, ok := f.h.streams.acquire("192.0.2.3"); ok {
		stream.release()
		t.Fatal("generation publication reopened permanently closed streams")
	}
}

func TestDatabaseSwitchFailedPublicReloadKeepsAdmissionClosed(t *testing.T) {
	f := newConsoleLiveFixture(t)
	if _, err := f.h.management.SetPublicPageEnabled(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.h.PrepareDatabaseSwitch(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := f.h.nodes.Database().Acquire(ctx, storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	failed, stop := context.WithCancel(lease.Context())
	stop()
	if err = f.h.PublishDatabaseGeneration(failed, lease); !errors.Is(err, context.Canceled) {
		t.Fatalf("reload ignored cancellation: %v", err)
	}
	if stream, ok := f.h.streams.acquire("192.0.2.1"); ok {
		stream.release()
		t.Fatal("failed reload reopened streams")
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes", nil))
	if w.Code != 503 {
		t.Fatalf("failed public reload exposed data status=%d", w.Code)
	}
	if err = f.h.PublishDatabaseGeneration(lease.Context(), lease); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes", nil))
	if w.Code != 403 {
		t.Fatalf("new disabled setting not applied status=%d", w.Code)
	}
}
