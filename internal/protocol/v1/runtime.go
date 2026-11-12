package v1

const (
	RemoteCommandUpgrade = "upgrade"
	RemoteCommandRestart = "restart"
	RemoteCommandStop    = "stop"
	FeatureDisk          = "disk"
	FeatureDiskIO        = "disk_io"
	FeaturePressure      = "pressure"
	FeatureTemperature   = "temperature"
	FeatureNetwork       = "network"
	FeatureTraffic       = "traffic"
	FeatureProbes        = "probes"
)

// RemoteCommandOrder 返回远程指令的规范顺序，调用方可安全修改返回值。
func RemoteCommandOrder() []string {
	return []string{RemoteCommandUpgrade, RemoteCommandRestart, RemoteCommandStop}
}

// DisabledFeatureOrder 返回可关闭功能的规范顺序。
func DisabledFeatureOrder() []string {
	return []string{FeatureDisk, FeatureNetwork, FeatureTraffic, FeatureProbes, FeatureDiskIO, FeaturePressure, FeatureTemperature}
}
