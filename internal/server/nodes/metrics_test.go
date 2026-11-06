package nodes_test

import (
	"context"
	"reflect"
	"testing"

	"orbitbeacon/internal/server/nodes"
)

func TestMetricsMetadataRecordsExcludeNotesAndJoinGroups(t *testing.T) {
	service, db := newNodes(t, 3)
	first := pending(t, service, "first")
	second := pending(t, service, "second")
	group := "00000000-0000-4000-8000-000000000099"
	if _, err := db.Exec(`INSERT INTO groups(id,name) VALUES(?,'fleet')`, group); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE nodes SET notes='invalid-json-notes',region='earth',provider='cloud',group_id=? WHERE id=?`, group, first.ID); err != nil {
		t.Fatal(err)
	}
	records, err := service.MetricsMetadataRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expected := []nodes.MetricsMetadata{{ID: first.ID, Name: "first", Region: "earth", Provider: "cloud", Group: "fleet"}, {ID: second.ID, Name: "second"}}
	if expected[0].ID > expected[1].ID {
		expected[0], expected[1] = expected[1], expected[0]
	}
	if !reflect.DeepEqual(records, expected) {
		t.Fatalf("records=%+v want %+v", records, expected)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.MetricsMetadataRecords(ctx); err == nil {
		t.Fatal("ignored context cancellation")
	}
}
