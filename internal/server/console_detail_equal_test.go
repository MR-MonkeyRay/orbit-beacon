package server

import (
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

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
