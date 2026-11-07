package announcements

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestDatabaseDrainWaitsForCommittedInvalidation(t *testing.T) {
	raw := testDB(t)
	db := mainHandle(t, raw)
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	defer unblock()
	service := New(db, func() time.Time { return testNow }, func() {
		var count int
		if err := raw.QueryRow("SELECT count(*) FROM announcements").Scan(&count); err != nil || count != 1 {
			t.Errorf("invalidation ran before commit: count=%d err=%v", count, err)
		}
		close(entered)
		<-release
	})
	saved := make(chan error, 1)
	input := patch(t, `{"level":"info","title":"Title","body":"Body"}`)
	go func() {
		_, err := service.Save(context.Background(), "", input)
		saved <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("invalidation did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- db.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("database drained during committed invalidation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-saved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("save did not finish")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("database did not drain after invalidation")
	}
}
