//go:build !linux

package services

import "testing"

func TestUnsupportedPingDoesNotResolveOrOfferPermissionHint(t *testing.T) {
	// Even an invalid hostname must report the platform capability, not a DNS error.
	result := NewChecker().Check(t.Context(), Service{Type: "icmp", Target: "invalid target", TimeoutSeconds: 1})
	if result.Error != "unsupported" || result.OK || result.LatencyMS != nil {
		t.Fatalf("unsupported ping: %+v", result)
	}
}
