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
	valid.Kind, valid.TargetVersion = "upgrade", "1.2.3"
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

func TestStrictVersionsAndComparison(t *testing.T) {
	for _, raw := range []string{"", "01.2.3", "1.02.3", "1.2.03", "1.2", "v1.2.3", "1.2.3-rc1", "../1.2.3", "1.2.3\n", "1.2.3+build"} {
		if _, err := v1.ParseVersion(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, pair := range [][2]string{{"0.0.0", "0.0.1"}, {"1.2.9", "1.2.10"}, {"1.9.99", "1.10.0"}, {"9.99.99", "10.0.0"}, {"1.2.3", "99999999999999999999999999999999999999.0.0"}} {
		a, err := v1.ParseVersion(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		b, err := v1.ParseVersion(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if a.Compare(b) != -1 || b.Compare(a) != 1 || a.Compare(a) != 0 {
			t.Fatal(pair)
		}
	}
	for _, code := range []string{"ok", "disabled_locally", "unsupported", "expired", "interrupted", "failed", "download", "checksum", "self_check", "downgrade", "rolled_back"} {
		if len(code) > 16 {
			t.Fatal(code)
		}
		if err := (v1.CommandResult{ID: "00000000-0000-4000-8000-000000000001", Status: "failed", Code: code}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	command := v1.Command{ID: "00000000-0000-4000-8000-000000000001", Kind: "upgrade", TargetVersion: "1.2.3", ExpiresAt: time.Now()}
	data, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		if key != "id" && key != "kind" && key != "target_version" && key != "expires_at" {
			t.Fatalf("unexpected command field %s", key)
		}
	}
}
