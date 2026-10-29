package v1

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const maxMetricSeries = 8

// UnmarshalJSON drops extensions before decoding typed metadata. Known scalar
// keys and disk/network families still use the complete validation contract.
func (m *Metrics) UnmarshalJSON(data []byte) error {
	type values Metrics
	var decoded values
	wire := struct {
		*values
		MetricStatus map[string]json.RawMessage `json:"metric_status"`
		Sources      map[string]json.RawMessage `json:"sources"`
	}{values: &decoded}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if len(decoded.Disks) > maxMetricSeries || len(decoded.Networks) > maxMetricSeries {
		return errors.New("too many metric series")
	}
	knownSeries := func(key string) (bool, error) {
		if strings.HasPrefix(key, "disk:") {
			for _, disk := range decoded.Disks {
				if key == DiskMetricKey(disk.Path) {
					return true, nil
				}
			}
			return false, fmt.Errorf("metric metadata references unreported series %s", key)
		}
		if strings.HasPrefix(key, "network:") {
			for _, network := range decoded.Networks {
				if key == NetworkMetricKey(network.Name) {
					return true, nil
				}
			}
			return false, fmt.Errorf("metric metadata references unreported series %s", key)
		}
		return false, nil
	}
	decoded.MetricStatus = make(map[string]string, len(metricStatusFields)+len(decoded.Disks)+len(decoded.Networks))
	for key, raw := range wire.MetricStatus {
		series, err := knownSeries(key)
		if err != nil {
			return err
		}
		if !series && !slices.Contains(metricStatusFields[:], key) {
			continue
		}
		var status string
		if err := json.Unmarshal(raw, &status); err != nil {
			return fmt.Errorf("metric status %s: %w", key, err)
		}
		decoded.MetricStatus[key] = status
	}
	decoded.Sources = make(map[string]SourceMetadata, len(sourceFamilies)+len(decoded.Disks)+len(decoded.Networks))
	for key, raw := range wire.Sources {
		series, err := knownSeries(key)
		if err != nil {
			return err
		}
		if !series && !slices.Contains(sourceFamilies[:], key) {
			continue
		}
		var source SourceMetadata
		if err := json.Unmarshal(raw, &source); err != nil {
			return fmt.Errorf("metric source %s: %w", key, err)
		}
		decoded.Sources[key] = source
	}
	*m = Metrics(decoded)
	return nil
}
