package v1_test

import (
	"encoding/json"
	v1 "orbitbeacon/internal/protocol/v1"
	"testing"
)

func TestDiskInodeValidation(t *testing.T) {
	total, used, zero := uint64(10), uint64(11), uint64(0)
	for _, tc := range []struct {
		name        string
		total, used *uint64
		valid       bool
	}{
		{"unsupported", nil, nil, true}, {"total only", &total, nil, false}, {"used only", nil, &total, false},
		{"exceeds total", &total, &used, false}, {"full", &total, &total, true}, {"empty", &total, &zero, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var report v1.Report
			if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
				t.Fatal(err)
			}
			report.Metrics.Disks[0].InodesTotal, report.Metrics.Disks[0].InodesUsed = tc.total, tc.used
			if err := report.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate()=%v valid=%v", err, tc.valid)
			}
		})
	}
}
