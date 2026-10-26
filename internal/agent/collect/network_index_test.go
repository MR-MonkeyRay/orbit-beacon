package collect_test

import (
	"testing"
	"time"

	"orbitbeacon/internal/agent/collect"
)

func TestNetworkDifferencerKeepsFixedBoundedBaselineIndex(t *testing.T) {
	var rates collect.NetworkDifferencer
	for i := range 8 {
		name := "eth" + string(rune('0'+i))
		if result := rates.Observe(name, collect.NetworkCounter{RxBytes: uint64(i), TxBytes: uint64(i)}, time.Duration(i+1)*time.Second); result.Network.RxBytesPerSec != nil {
			t.Fatalf("initial sample for %s invented rate", name)
		}
	}
	if result := rates.Observe("eth8", collect.NetworkCounter{RxBytes: 100, TxBytes: 100}, 9*time.Second); result.Network.RxBytesPerSec != nil {
		t.Fatal("new ninth interface reused an evicted baseline")
	}
	if result := rates.Observe("eth0", collect.NetworkCounter{RxBytes: 10, TxBytes: 10}, 10*time.Second); result.Network.RxBytesPerSec != nil {
		t.Fatal("oldest interface retained a baseline after bounded eviction")
	}
	result := rates.Observe("eth8", collect.NetworkCounter{RxBytes: 110, TxBytes: 120}, 10*time.Second)
	if result.Network.RxBytesPerSec == nil || result.Network.TxBytesPerSec == nil || *result.Network.RxBytesPerSec != 10 || *result.Network.TxBytesPerSec != 20 {
		t.Fatalf("retained ninth-interface baseline produced %+v", result.Network)
	}
}
