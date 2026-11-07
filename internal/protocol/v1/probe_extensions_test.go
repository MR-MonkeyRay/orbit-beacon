package v1_test

import (
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestDNSTargetValidation(t *testing.T) {
	for _, target := range []string{"dns://example.com", "dns://localhost", "dns://127.0.0.1", "dns://[::1]", "dns://example.com."} {
		if err := v1.ValidateProbeTarget(target); err != nil {
			t.Errorf("%s: %v", target, err)
		}
	}
	for _, target := range []string{"dns://", "dns://example.com:53", "dns://[::1]:53", "dns://example.com/", "dns://example.com/path", "dns://example.com?", "dns://example.com?q=1", "dns://user@example.com", "dns://example.com#", "dns://bad_host", "dns://-bad.example", "dns://999.1.1.1", "dns://example..com"} {
		if err := v1.ValidateProbeTarget(target); err == nil {
			t.Errorf("accepted %s", target)
		}
	}
}

func TestProbeTLSExpiryValidation(t *testing.T) {
	at := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	zero := time.Time{}
	latency := 1.0
	for _, target := range []string{"https://example.com", "http://example.com", "tcp://example.com:443", "icmp://example.com", "dns://example.com"} {
		for _, expiry := range []*time.Time{nil, &zero, &at} {
			p := v1.ProbeResult{Target: target, OK: true, LatencyMS: &latency, CheckedAt: at, TLSExpiresAt: expiry}
			valid := expiry == nil || target == "https://example.com" && expiry == &at
			if err := p.Validate(); (err == nil) != valid {
				t.Errorf("%s expiry=%v valid=%t: %v", target, expiry, valid, err)
			}
		}
	}
	code := 302
	if err := (v1.ProbeResult{Target: "https://example.com", Error: "http_status", StatusCode: &code, CheckedAt: at, TLSExpiresAt: &at}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProbeTimesMustStayStorableInUTC(t *testing.T) {
	at := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	latency := 1.0
	last := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	later := time.Date(9999, 12, 31, 23, 59, 59, 0, time.FixedZone("", -60))
	first := time.Date(1, 1, 1, 0, 0, 1, 0, time.UTC)
	earlier := time.Date(1, 1, 1, 0, 0, 1, 0, time.FixedZone("", 2))
	for _, test := range []struct {
		value time.Time
		valid bool
	}{{last, true}, {later, false}, {first, true}, {earlier, false}} {
		expiry := test.value
		if err := (v1.ProbeResult{Target: "https://example.com", OK: true, LatencyMS: &latency, CheckedAt: at, TLSExpiresAt: &expiry}).Validate(); (err == nil) != test.valid {
			t.Errorf("expiry %s valid=%t: %v", test.value, test.valid, err)
		}
		if err := (v1.ProbeResult{Target: "tcp://example.com:443", OK: true, LatencyMS: &latency, CheckedAt: test.value}).Validate(); (err == nil) != test.valid {
			t.Errorf("checked %s valid=%t: %v", test.value, test.valid, err)
		}
	}
}
