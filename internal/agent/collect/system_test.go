package collect_test

import (
	"context"
	"testing"

	"orbitbeacon/internal/agent/collect"
	"orbitbeacon/internal/buildinfo"
)

func TestSystemIncludesAgentVersion(t *testing.T) {
	system, err := collect.System(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if system.AgentVersion != buildinfo.Version {
		t.Fatalf("agent version = %q, want %q", system.AgentVersion, buildinfo.Version)
	}
	if err := system.Validate(); err != nil {
		t.Fatalf("collected system is invalid: %v", err)
	}
}
