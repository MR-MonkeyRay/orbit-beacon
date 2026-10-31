package v1

const (
	MetricCPUPercent           = "cpu_percent"
	MetricLoad1                = "load1"
	MetricLoad5                = "load5"
	MetricLoad15               = "load15"
	MetricMemoryTotalBytes     = "memory_total_bytes"
	MetricMemoryAvailableBytes = "memory_available_bytes"
	MetricSwapTotalBytes       = "swap_total_bytes"
	MetricSwapUsedBytes        = "swap_used_bytes"
	MetricUptimeSeconds        = "uptime_seconds"
	SourceCPU                  = "cpu"
	SourceLoad                 = "load"
	SourceMemory               = "memory"
	SourceSwap                 = "swap"
	SourceUptime               = "uptime"
)

var metricStatusFields = [...]string{
	MetricCPUPercent, MetricLoad1, MetricLoad5, MetricLoad15,
	MetricMemoryTotalBytes, MetricMemoryAvailableBytes,
	MetricSwapTotalBytes, MetricSwapUsedBytes, MetricUptimeSeconds,
}

var sourceFamilies = [...]string{SourceCPU, SourceLoad, SourceMemory, SourceSwap, SourceUptime}

// MetricStatusFields 和 SourceFamilies 是只读的传输协议字段清单。
// 两者都按值返回数组，调用方无法改动包级契约。
func MetricStatusFields() [9]string { return metricStatusFields }
func SourceFamilies() [5]string     { return sourceFamilies }

func DiskMetricKey(path string) string    { return "disk:" + path }
func NetworkMetricKey(name string) string { return "network:" + name }
