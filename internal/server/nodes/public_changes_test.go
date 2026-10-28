package nodes_test

import (
	"testing"

	"orbitbeacon/internal/server/nodes"
)

func TestInvalidatePublicBroadcastsAndAdvancesVisibilityVersion(t *testing.T) {
	service := nodes.NewService(nil, nodes.Options{})
	version, changed := service.PublicChanges()
	visibility := service.PublicVisibilityVersion()
	service.InvalidatePublic()
	select {
	case <-changed:
	default:
		t.Fatal("public invalidation did not broadcast")
	}
	next, _ := service.PublicChanges()
	if next != version+1 {
		t.Fatalf("change version = %d, want %d", next, version+1)
	}
	if got := service.PublicVisibilityVersion(); got != visibility+1 {
		t.Fatalf("visibility version = %d, want %d", got, visibility+1)
	}
}
