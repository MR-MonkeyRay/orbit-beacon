package v1_test

import (
	"encoding/json"
	"math"
	"slices"
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestOptionalHostMetricsValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"disk_io":null,"pressure":null,"temperature_celsius":null}`, `{"pressure":{"cpu":null,"memory":12.5,"io":null}}`} {
		m := completeMetrics(t)
		body, _ := json.Marshal(m)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		for _, key := range []string{"disk_io", "pressure", "temperature_celsius"} {
			delete(fields, key)
		}
		var extra map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &extra)
		for k, v := range extra {
			fields[k] = v
		}
		body, _ = json.Marshal(fields)
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	setters := []struct {
		name  string
		limit float64
		set   func(*v1.Metrics, float64)
	}{
		{"read", 1e15, func(m *v1.Metrics, v float64) { m.DiskIO = &v1.DiskIOMetrics{ReadBytesPerSec: v} }},
		{"write", 1e15, func(m *v1.Metrics, v float64) { m.DiskIO = &v1.DiskIOMetrics{WriteBytesPerSec: v} }},
		{"busy", 100, func(m *v1.Metrics, v float64) { m.DiskIO = &v1.DiskIOMetrics{BusyPercent: v} }},
		{"cpu", 100, func(m *v1.Metrics, v float64) { m.Pressure = &v1.PressureMetrics{CPU: &v} }},
		{"memory", 100, func(m *v1.Metrics, v float64) { m.Pressure = &v1.PressureMetrics{Memory: &v} }},
		{"io", 100, func(m *v1.Metrics, v float64) { m.Pressure = &v1.PressureMetrics{IO: &v} }},
		{"temperature", 150, func(m *v1.Metrics, v float64) { m.TemperatureCelsius = &v }},
	}
	for _, tc := range setters {
		t.Run(tc.name, func(t *testing.T) {
			for _, v := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1), tc.limit * 1.01} {
				m := completeMetrics(t)
				tc.set(&m, v)
				if m.Validate() == nil {
					t.Fatalf("accepted %v", v)
				}
			}
			m := completeMetrics(t)
			tc.set(&m, tc.limit)
			if err := m.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.set(&m, 0)
			if (m.Validate() == nil) != (tc.name != "temperature") {
				t.Fatal("zero boundary")
			}
		})
	}
}

func TestHostMetricFeatureOrderAndDiskImplication(t *testing.T) {
	want := []string{"disk", "network", "traffic", "probes", "disk_io", "pressure", "temperature"}
	if !slices.Equal(v1.DisabledFeatureOrder(), want) {
		t.Fatal(v1.DisabledFeatureOrder())
	}
	config := v1.DefaultRuntimeSettings()
	config.DisabledFeatures = []string{v1.FeatureDisk}
	if !config.FeatureDisabled(v1.FeatureDiskIO) || config.FeatureDisabled(v1.FeaturePressure) {
		t.Fatal("disk implication")
	}
	config.DisabledFeatures = []string{v1.FeatureDiskIO}
	if config.FeatureDisabled(v1.FeatureDisk) {
		t.Fatal("reverse implication")
	}
}
