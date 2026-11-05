package server

import (
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestConsoleDetailsEqualSeesLocalAgentSwitches(t *testing.T) {
	base, same, changed := v1.DefaultRuntimeSettings(), v1.DefaultRuntimeSettings(), v1.DefaultRuntimeSettings()
	changed.DisabledFeatures = []string{v1.FeatureDisk}
	if !consoleDetailsEqual(consoleDetailData{Runtime: &base}, consoleDetailData{Runtime: &same}) {
		t.Fatal("identical runtime reported as changed")
	}
	if consoleDetailsEqual(consoleDetailData{Runtime: &base}, consoleDetailData{Runtime: &changed}) {
		t.Fatal("console detail ignored a disabled feature")
	}
}
