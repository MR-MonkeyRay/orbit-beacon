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

func TestConsoleDetailsEqualSeesExpiryDate(t *testing.T) {
	first, second := "2027-01-01", "2027-02-01"
	unset, set, renewed := nodes.Node{}, nodes.Node{ExpiresOn: &first}, nodes.Node{ExpiresOn: &second}
	if consoleDetailsEqual(consoleDetailData{Node: &unset}, consoleDetailData{Node: &set}) {
		t.Fatal("console detail ignored a new expiry date")
	}
	if consoleDetailsEqual(consoleDetailData{Node: &set}, consoleDetailData{Node: &renewed}) {
		t.Fatal("console detail ignored a changed expiry date")
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

func TestConsoleDetailsDetectsNewOptionalHostSample(t *testing.T) {
	for _, metrics := range []v1.Metrics{
		{DiskIO: &v1.DiskIOMetrics{ReadBytesPerSec: 1024}},
		{Pressure: &v1.PressureMetrics{CPU: new(1.5)}},
		{TemperatureCelsius: new(45.0)},
	} {
		before := consoleDetailData{Latest: &latestSampleView{Seq: 1}}
		after := consoleDetailData{Latest: &latestSampleView{Seq: 2, Metrics: metrics}}
		if consoleDetailsEqual(before, after) {
			t.Fatal("new host sample ignored")
		}
	}
}
