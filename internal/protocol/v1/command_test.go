package v1_test

import (
	"encoding/json"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestClosedCommandValidation(t *testing.T) {
	valid := v1.Command{ID: "00000000-0000-4000-8000-000000000001", Kind: "restart", ExpiresAt: time.Now()}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*v1.Command){func(c *v1.Command) { c.ID = "x" }, func(c *v1.Command) { c.Kind = "shell" }, func(c *v1.Command) { c.TargetVersion = "x" }, func(c *v1.Command) { c.ExpiresAt = time.Time{} }, func(c *v1.Command) { c.Kind = "upgrade" }} {
		c := valid
		mutate(&c)
		if c.Validate() == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	valid.Kind, valid.TargetVersion = "upgrade", "v1.2.3"
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, r := range []v1.CommandResult{{ID: valid.ID, Status: "succeeded", Code: "shell output"}, {ID: valid.ID, Status: "output", Code: "ok"}, {ID: "bad", Status: "failed", Code: "failed"}} {
		if r.Validate() == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
}

func TestReportCommandResultsBound(t *testing.T) {
	var report v1.Report
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		report.CommandResults = append(report.CommandResults, v1.CommandResult{ID: "00000000-0000-4000-8000-000000000001", Status: "rejected", Code: "failed"})
		err := report.Validate()
		if (i < 2) != (err == nil) {
			t.Fatalf("count %d: %v", i+1, err)
		}
	}
}
