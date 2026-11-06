package server

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/history"
)

func TestTokenHostMetricsWhitelist(t *testing.T) {
	value := 45.5
	metric := v1.Metrics{DiskIO: &v1.DiskIOMetrics{ReadBytesPerSec: 1024, WriteBytesPerSec: 2048, BusyPercent: 25}, Pressure: &v1.PressureMetrics{CPU: &value}, TemperatureCelsius: &value}
	body, err := json.Marshal(tokenMetricsDTO(metric))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"disk_io", "pressure", "temperature_celsius"} {
		if string(fields[name]) == "null" || fields[name] == nil {
			t.Fatal(name)
		}
	}
	original, _ := json.Marshal(metric)
	if string(original) != string(body) {
		t.Fatalf("token telemetry mismatch %s", body)
	}
	metricType := reflect.TypeOf(metric)
	if len(fields) != metricType.NumField() {
		t.Fatal("whitelist incomplete")
	}
	response, err := history.Empty("temperature_celsius", "1h")
	if err != nil {
		t.Fatal(err)
	}
	if tokenHistoryDTO(response).Unit != "celsius" {
		t.Fatal("token temperature unit")
	}
}

func TestOpenAPIHostMetricContract(t *testing.T) {
	body, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	metrics := schemas["Metrics"].(map[string]any)
	props := metrics["properties"].(map[string]any)
	for _, field := range []string{"disk_io", "pressure", "temperature_celsius"} {
		schema, ok := props[field].(map[string]any)
		if !ok {
			t.Fatal(field)
		}
		types := schema["type"].([]any)
		if !slices.Contains(types, any("null")) {
			t.Fatal(field, "not nullable")
		}
		for _, required := range metrics["required"].([]any) {
			if required == field {
				t.Fatal(field, "must stay optional")
			}
		}
	}
	if props["temperature_celsius"].(map[string]any)["maximum"] != 150 {
		t.Fatal("temperature maximum")
	}
	response := schemas["MetricHistory"].(map[string]any)["properties"].(map[string]any)
	enum := response["metric"].(map[string]any)["enum"].([]any)
	for _, metric := range []string{"disk_read_bytes_per_second", "disk_write_bytes_per_second", "disk_busy_percent", "pressure_cpu_percent", "pressure_memory_percent", "pressure_io_percent", "temperature_celsius"} {
		if !slices.Contains(enum, any(metric)) || !history.ValidMetric(metric) {
			t.Fatal(metric)
		}
	}
	if !slices.Contains(response["unit"].(map[string]any)["enum"].([]any), any("celsius")) {
		t.Fatal("celsius unit")
	}
}
