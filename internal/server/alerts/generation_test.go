package alerts

import (
	"context"
	"testing"
	"time"
)

func TestEvaluatorNewDatabaseTimelineDoesNotReusePendingDuration(t *testing.T) {
	f := newFixture(t, 60)
	f.tick(t)
	f.advance(61)
	f.value(90)
	next := NewEvaluator(f.s, f.latest, EvaluatorOptions{})
	next.pending = f.e.pending
	next.resolutions = f.e.resolutions
	if err := next.Evaluate(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	if got := f.events(t, "firing"); len(got) != 0 {
		t.Fatal("old timeline pending fired in newly initialized evaluator", got)
	}
	f.advance(61)
	f.value(90)
	if err := next.Evaluate(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	got := f.events(t, "firing")
	if len(got) != 1 || got[0].StartedAt.Before(f.now.Add(-61*time.Second)) {
		t.Fatal("new timeline did not start its own duration", got)
	}
}
