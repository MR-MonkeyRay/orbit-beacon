package v1_test

import (
	"encoding/json"
	"fmt"
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestTelemetryDecoderBoundsReportedSeriesBeforeMetadata(t *testing.T) {
	for _, family := range []string{"disk", "network"} {
		for _, count := range []int{8, 9} {
			t.Run(fmt.Sprintf("%s/%d", family, count), func(t *testing.T) {
				metrics := completeMetrics(t)
				for index := 1; index < count; index++ {
					var key, original string
					if family == "disk" {
						series := metrics.Disks[0]
						original = v1.DiskMetricKey(series.Path)
						series.Path = fmt.Sprintf("/series-%d", index)
						key = v1.DiskMetricKey(series.Path)
						metrics.Disks = append(metrics.Disks, series)
					} else {
						series := metrics.Networks[0]
						original = v1.NetworkMetricKey(series.Name)
						series.Name = fmt.Sprintf("eth%d", index)
						key = v1.NetworkMetricKey(series.Name)
						metrics.Networks = append(metrics.Networks, series)
					}
					metrics.MetricStatus[key] = metrics.MetricStatus[original]
					metrics.Sources[key] = metrics.Sources[original]
				}
				body, err := json.Marshal(metrics)
				if err != nil {
					t.Fatal(err)
				}
				var decoded v1.Metrics
				err = json.Unmarshal(body, &decoded)
				if count == 9 {
					if err == nil {
						t.Fatal("oversized series reached typed metadata decoding")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := decoded.Validate(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
