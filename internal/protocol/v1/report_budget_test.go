package v1

import (
	"encoding/json"
	"testing"
	"time"
)

func TestProbeResultEnvelopeCoversTLSExpiry(t *testing.T) {
	at := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("widest", 23*3600+59*60))
	latency, code := 0.00010000000000000002, 599
	target := "https://example.test"
	result := ProbeResult{Target: target, OK: true, LatencyMS: &latency, StatusCode: &code, CheckedAt: at, TLSExpiresAt: &at}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	targetJSON, _ := json.Marshal(target)
	overhead := len(data) - len(targetJSON)
	if overhead > probeResultEnvelopeBytes {
		t.Fatalf("probe envelope=%d exceeds %d: %s", overhead, probeResultEnvelopeBytes, data)
	}
	t.Logf("widest probe fixed fields: %d bytes; reservation: %d bytes", overhead, probeResultEnvelopeBytes)
}
