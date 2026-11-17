package v1

// 协议内置的标量指标字段名与采集源族名称常量。
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

// MetricStatusFields 返回内置标量指标状态键的只读副本。
func MetricStatusFields() [9]string { return metricStatusFields }

// SourceFamilies 返回内置基础采集源族名称的只读副本。
func SourceFamilies() [5]string { return sourceFamilies }

// DiskMetricKey 返回指定挂载路径在 metric_status 与 sources 中使用的标准键名。
func DiskMetricKey(path string) string { return "disk:" + path }

// NetworkMetricKey 返回指定网卡名称在 metric_status 与 sources 中使用的标准键名。
func NetworkMetricKey(name string) string { return "network:" + name }

// DisabledStatusFields are optional report signals for locally disabled collectors.
func DisabledStatusFields() [7]string {
	return [7]string{"disk", "network", "traffic", "probes", "disk_io", "pressure", "temperature"}
}
