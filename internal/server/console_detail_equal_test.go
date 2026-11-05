package server

import (
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
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

func TestConsoleLiveComparisonsSeeStoppedNodes(t *testing.T) {
	running, stopped := nodes.Node{}, nodes.Node{Stopped: true}
	if consoleDetailsEqual(consoleDetailData{Node: &running}, consoleDetailData{Node: &stopped}) {
		t.Fatal("console detail ignored a stopped node")
	}
	if consoleFleetsEqual(consoleFleetData{Nodes: []nodes.LiveNode{{}}}, consoleFleetData{Nodes: []nodes.LiveNode{{Stopped: true}}}) {
		t.Fatal("console fleet ignored a stopped node")
	}
}
