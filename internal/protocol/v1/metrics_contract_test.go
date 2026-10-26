package v1

import "testing"

func TestMetricContractExportsStableFieldAndSourceKeys(t *testing.T) {
	if got := MetricStatusFields(); got != [...]string{MetricCPUPercent, MetricLoad1, MetricLoad5, MetricLoad15, MetricMemoryTotalBytes, MetricMemoryAvailableBytes, MetricSwapTotalBytes, MetricSwapUsedBytes, MetricUptimeSeconds} {
		t.Fatalf("metric status contract changed: %v", got)
	}
	if got := SourceFamilies(); got != [...]string{SourceCPU, SourceLoad, SourceMemory, SourceSwap, SourceUptime} {
		t.Fatalf("source family contract changed: %v", got)
	}
	if DiskMetricKey("/") != "disk:/" || NetworkMetricKey("eth0") != "network:eth0" {
		t.Fatal("series key contract changed")
	}
}
