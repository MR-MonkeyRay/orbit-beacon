package services

import (
	"encoding/json"
	"testing"
)

func TestPatchGroupIDTriState(t *testing.T) {
	for _, tc := range []struct {
		body    string
		present bool
		valid   bool
	}{
		{`{}`, false, true}, {`{"group_id":null}`, true, true}, {`{"group_id":""}`, true, true},
		{`{"group_id":"00000000-0000-4000-8000-000000000001"}`, true, true},
		{`{"enabled":null}`, false, false}, {`{"unknown":null}`, false, false}, {`{"unknown":1}`, false, false},
	} {
		var p Patch
		err := json.Unmarshal([]byte(tc.body), &p)
		if (err == nil) != tc.valid {
			t.Fatalf("%s err=%v", tc.body, err)
		}
		if err == nil && (p.GroupID != nil) != tc.present {
			t.Fatalf("%s group presence=%t", tc.body, p.GroupID != nil)
		}
	}
}

func TestServiceGroupConfigFence(t *testing.T) {
	a, b := validService(), validService()
	a.GroupID = ptr("00000000-0000-4000-8000-000000000001")
	b.GroupID = ptr(*a.GroupID)
	if !sameConfig(a, b) {
		t.Fatal("group compared by pointer identity")
	}
	b.GroupID = nil
	if sameConfig(a, b) {
		t.Fatal("stale group configuration accepted")
	}
	if err := Validate(a); err != nil {
		t.Fatal(err)
	}
	a.GroupID = ptr("bad")
	if err := Validate(a); err == nil {
		t.Fatal("invalid group UUID accepted")
	}
}

func TestServiceGroupDeleteRefreshesConfigsAndPreservesResults(t *testing.T) {
	m, _, _ := fixture(t, nil)
	group, err := m.SaveGroup(t.Context(), "", "group")
	if err != nil {
		t.Fatal(err)
	}
	s := create(t, m)
	s, err = m.Save(t.Context(), s.ID, Patch{GroupID: json.RawMessage(`"` + group.ID + `"`)})
	if err != nil {
		t.Fatal(err)
	}
	latency := 10.0
	if err = m.Accept(t.Context(), s, Result{OK: true, LatencyMS: &latency, CheckedAt: m.now()}); err != nil {
		t.Fatal(err)
	}
	if err = m.DeleteGroup(t.Context(), group.ID); err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(t.Context(), s.ID)
	if err != nil || got.GroupID != nil || got.Latest == nil || !got.Latest.OK {
		t.Fatalf("refreshed service=%+v err=%v", got, err)
	}
	configs, err := m.configs(t.Context())
	if err != nil || len(configs) != 1 || configs[0].GroupID != nil {
		t.Fatalf("scheduler configs=%+v err=%v", configs, err)
	}
	if err = m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err = m.Get(t.Context(), s.ID)
	if err != nil || got.GroupID != nil || got.Latest == nil || !got.Latest.OK {
		t.Fatalf("flushed service=%+v err=%v", got, err)
	}
}
