package services

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPublicServiceGroupsPrivacyOrderMappingAndChanges(t *testing.T) {
	m, _, _ := fixture(t, nil)
	ctx := t.Context()
	signals := 0
	m.changed = func() { signals++ }
	a, err := m.SaveGroup(ctx, "", "Same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.SaveGroup(ctx, "", "Same\u200b")
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := m.SaveGroup(ctx, "", "Hidden")
	if err != nil {
		t.Fatal(err)
	}
	if signals != 3 {
		t.Fatalf("group create signals: %d", signals)
	}
	group := func(id string) json.RawMessage { return json.RawMessage(`"` + id + `"`) }
	first := create(t, m)
	second := create(t, m)
	third := create(t, m)
	private := create(t, m)
	paused := create(t, m)
	for _, tc := range []struct {
		id    string
		patch Patch
	}{
		{first.ID, Patch{GroupID: group(a.ID)}}, {second.ID, Patch{GroupID: group(b.ID)}},
		{private.ID, Patch{GroupID: group(hidden.ID), Public: ptr(false)}},
		{paused.ID, Patch{GroupID: group(hidden.ID), Enabled: ptr(false)}},
	} {
		if _, err := m.Save(ctx, tc.id, tc.patch); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.ReorderGroups(ctx, []string{hidden.ID, b.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	verify := func(names []string, indexes map[string]*int) {
		t.Helper()
		got, groups, err := m.PublicAtWithGroups(ctx, m.now(), true)
		if err != nil || !reflect.DeepEqual(groups, names) || len(got) != 3 {
			t.Fatalf("public groups: %+v %v %v", got, groups, err)
		}
		// All service names happen to match; use the same stable ordering as the public projection.
		configs, err := m.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		i := 0
		for _, config := range configs {
			if !config.Enabled || !config.Public {
				continue
			}
			if got[i].Group == nil || !reflect.DeepEqual(*got[i].Group, indexes[config.ID]) {
				t.Fatalf("group of %s: %+v", config.ID, got[i].Group)
			}
			i++
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{a.ID, b.ID, hidden.ID, "group_id", "Hidden"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("leaked %q: %s", secret, raw)
			}
		}
	}
	verify([]string{b.Name, a.Name}, map[string]*int{first.ID: ptr(1), second.ID: ptr(0), third.ID: nil})
	before := signals
	if _, err := m.SaveGroup(ctx, a.ID, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if signals != before+1 {
		t.Fatal("rename did not signal")
	}
	verify([]string{b.Name, "Renamed"}, map[string]*int{first.ID: ptr(1), second.ID: ptr(0)})
	before = signals
	if _, err := m.ReorderGroups(ctx, []string{a.ID, hidden.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	if signals != before+1 {
		t.Fatal("reorder did not signal")
	}
	verify([]string{"Renamed", b.Name}, map[string]*int{first.ID: ptr(0), second.ID: ptr(1)})
	if _, err := m.Save(ctx, first.ID, Patch{GroupID: group(b.ID)}); err != nil {
		t.Fatal(err)
	}
	verify([]string{b.Name}, map[string]*int{first.ID: ptr(0), second.ID: ptr(0)})
	if err := m.DeleteGroup(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	verify([]string{}, map[string]*int{})
	off, err := m.Public(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(off)
	if err != nil || strings.Contains(string(raw), `"group"`) {
		t.Fatalf("off groups: %s %v", raw, err)
	}
}
