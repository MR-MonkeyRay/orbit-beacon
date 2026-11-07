package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
)

func TestConsoleFleetsEqualComparesMetricValues(t *testing.T) {
	fleet := func(cpu, memory *float64) consoleFleetData {
		return consoleFleetData{Nodes: []nodes.LiveNode{{ID: "node", CPUPercent: cpu, MemoryUsedPercent: memory}}}
	}
	for _, tc := range []struct {
		name  string
		a, b  consoleFleetData
		equal bool
	}{
		{"null", fleet(nil, nil), fleet(nil, nil), true},
		{"different-pointers-same-value", fleet(new(0.0), new(75.0)), fleet(new(0.0), new(75.0)), true},
		{"cpu-changed", fleet(new(0.0), new(75.0)), fleet(new(1.0), new(75.0)), false},
		{"memory-changed", fleet(new(0.0), new(75.0)), fleet(new(0.0), new(76.0)), false},
		{"cpu-null", fleet(new(0.0), nil), fleet(nil, nil), false},
		{"memory-null", fleet(nil, new(0.0)), fleet(nil, nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if consoleFleetsEqual(tc.a, tc.b) != tc.equal || consoleFleetsEqual(tc.b, tc.a) != tc.equal {
				t.Fatal("incorrect fleet metric equality")
			}
		})
	}
}

func TestConsoleFleetTwoHundredNodeFrameSize(t *testing.T) {
	// Longest permitted version, UUID, offset timestamp, and long finite percentages.
	seen := time.Date(2026, 10, 27, 12, 0, 0, 123456789, time.FixedZone("offset", 8*3600))
	data := consoleFleetData{Nodes: make([]nodes.LiveNode, 200)}
	for i := range data.Nodes {
		data.Nodes[i] = nodes.LiveNode{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), Status: "disabled", Presence: "disabled", LastSeen: &seen, AgentVersion: strings.Repeat("v", 64), CPUPercent: new(0.0000010000000000000002), MemoryUsedPercent: new(0.0000010000000000000002)}
	}
	frame := liveFrame[consoleFleetData]{Type: "snapshot", ServerTime: seen.UTC(), UpdateIntervalSeconds: 10, Data: &data}
	body, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(body, &legacy); err != nil {
		t.Fatal(err)
	}
	for _, item := range legacy["data"].(map[string]any)["nodes"].([]any) {
		delete(item.(map[string]any), "cpu_percent")
		delete(item.(map[string]any), "memory_used_percent")
	}
	old, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	// SSE parser's per-event UTF-8 budget includes event/data labels and line endings.
	sseBytes := len("event: message\ndata: ") + len(body) + len("\n\n")
	if sseBytes > 1024*1024 {
		t.Fatalf("200-node SSE event exceeds 1 MiB: %d", sseBytes)
	}
	t.Logf("200 nodes: legacy JSON=%d, metric JSON=%d, delta=%d, SSE=%d / %d bytes", len(old), len(body), len(body)-len(old), sseBytes, 1024*1024)
}
