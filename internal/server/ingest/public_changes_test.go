package ingest

import (
	"context"
	"testing"
	"time"
)

func TestAcceptedReportBroadcastsPublicChange(t *testing.T) {
	fixture := newCoalesceFixture(t, 30*time.Second)
	version, changed := fixture.service.PublicChanges()
	fixture.report.Seq++
	fixture.now = fixture.now.Add(time.Second)
	if _, err := fixture.service.Accept(context.Background(), fixture.id, fixture.report); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("accepted report did not broadcast")
	}
	next, _ := fixture.service.PublicChanges()
	if next != version+1 {
		t.Fatalf("change version = %d, want %d", next, version+1)
	}
}
