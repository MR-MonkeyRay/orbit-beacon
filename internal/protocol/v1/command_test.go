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
	for _, mutate := range []func(*v1.Command){func(c *v1.Command) { c.ID = "x" }, func(c *v1.Command) { c.Kind = "shell" }, func(c *v1.Command) { c.ExpiresAt = time.Time{} }} {
		c := valid
		mutate(&c)
		if c.Validate() == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	valid.Kind = "upgrade"
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
	command := v1.Command{ID: "00000000-0000-4000-8000-000000000001", Kind: "upgrade", ExpiresAt: time.Now()}
	data, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		if key != "id" && key != "kind" && key != "expires_at" {
			t.Fatalf("unexpected command field %s", key)
		}
	}
}

func TestFixedUpdateCommandsRejectServerParameters(t *testing.T) {
	for _, kind := range []string{"check_update", "upgrade", "restart", "stop"} {
		body := `{"id":"00000000-0000-4000-8000-000000000001","kind":"` + kind + `","expires_at":"2030-01-01T00:00:00Z"}`
		var command v1.Command
		if err := json.Unmarshal([]byte(body), &command); err != nil {
			t.Fatal(err)
		}
		if err := command.Validate(); err != nil {
			t.Errorf("fixed %s rejected: %v", kind, err)
		}
		for _, parameter := range []string{`"target_version":"1.2.3"`, `"target_version":null`, `"url":"https://attacker.test/agent"`, `"args":"sh"`, `"hash":"abcd"`} {
			injected := body[:len(body)-1] + "," + parameter + "}"
			if err := json.Unmarshal([]byte(injected), &command); err == nil {
				t.Errorf("server parameter accepted: %s", injected)
			}
		}
	}
}
