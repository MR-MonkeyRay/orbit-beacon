package availability

import (
	"context"
	"testing"
	"time"
)

func TestAvailabilityNewDatabaseTimelineDoesNotBackfillFailureGap(t *testing.T) {
	s, db, now := fixture(t)
	add(t, db, "online", *now)
	tick(t, s)
	*now = now.Add(time.Minute)
	tick(t, s)
	want(t, db, "online", 60, 0)
	replacement := NewService(s.db, s.now, nil)
	replacement.started = s.started
	replacement.previous = s.previous
	*now = now.Add(time.Hour)
	if err := replacement.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	want(t, db, "online", 60, 0)
	*now = now.Add(time.Minute)
	if err := replacement.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	want(t, db, "online", 60, 0)
}
