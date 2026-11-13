package ingest

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/services"
)

func TestPublicGroupsWireOmissionAndNull(t *testing.T) {
	at := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	page := PublicPage{Services: []services.PublicService{{Name: "Health", Type: "tcp", Status: "unknown", Availability: []*float64{}}}, Nodes: []PublicNode{{Name: "Node", Status: "unknown"}}, GeneratedAt: at}
	off, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"services":[{"name":"Health","type":"tcp","status":"unknown","latency_ms":null,"availability":[]}],"nodes":[{"last_seen":null,"name":"Node","status":"unknown"}],"has_more":false,"next_offset":null,"generated_at":"2026-11-13T00:00:00Z"}`
	if string(off) != want {
		t.Fatalf("off shape changed:\n%s", off)
	}
	names := []string{}
	page.NodeGroups, page.ServiceGroups = &names, &names
	var group *int
	page.Nodes[0].Group, page.Services[0].Group = &group, &group
	on, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"node_groups":[]`, `"service_groups":[]`, `"group":null`} {
		if !strings.Contains(string(on), key) {
			t.Fatalf("missing %s in %s", key, on)
		}
	}
	group = new(0)
	on, err = json.Marshal(page)
	if err != nil || strings.Count(string(on), `"group":0`) != 2 {
		t.Fatalf("zero index omitted: %s %v", on, err)
	}
}

func TestPublicNodeGroupsPaginationPrivacyAndCacheInvalidation(t *testing.T) {
	f := publicPageFixture(t, 4)
	ctx := context.Background()
	m := management.NewService(f.nodes.Database())
	m.SetPublicInvalidator(f.nodes.InvalidatePublic)
	a, err := m.SaveGroup(ctx, "", "Same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.SaveGroup(ctx, "", "Same\u200b") // visually similar names remain separate identities
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := m.SaveGroup(ctx, "", "Hidden")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE nodes SET public_settings='{"enabled":true,"fields":[]}',display_order=0,group_id=NULL`,
		`UPDATE nodes SET name='A',display_order=4,group_id='` + a.ID + `' WHERE name='coalesce'`,
		`UPDATE nodes SET name='B',display_order=3,group_id='` + b.ID + `' WHERE name='private-1'`,
		`UPDATE nodes SET name='C',display_order=2 WHERE name='private-2'`,
		`UPDATE nodes SET public_settings='{"enabled":false}',group_id='` + hidden.ID + `' WHERE name='private-3'`,
	} {
		if _, err := f.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.ReorderGroups(ctx, []string{hidden.ID, b.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return f.now }
	get := func(offset int, show bool) PublicPage {
		t.Helper()
		page, err := f.service.PublicPageWithGroups(ctx, f.nodes, 1, offset, time.Hour, clock, show)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{a.ID, b.ID, hidden.ID, "Hidden", "group_id"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("leaked %q: %s", secret, raw)
			}
		}
		return page
	}
	check := func(page PublicPage, names []string, index *int) {
		t.Helper()
		if page.NodeGroups == nil || !reflect.DeepEqual(*page.NodeGroups, names) || len(page.Nodes) != 1 || page.Nodes[0].Group == nil || !reflect.DeepEqual(*page.Nodes[0].Group, index) {
			t.Fatalf("bad groups: %+v node=%+v", page, page.Nodes)
		}
	}
	zero, one := 0, 1
	check(get(0, true), []string{b.Name, a.Name}, &one)
	check(get(1, true), []string{b.Name, a.Name}, &zero)
	check(get(2, true), []string{b.Name, a.Name}, nil)
	off := get(0, false)
	if off.NodeGroups != nil || off.Nodes[0].Group != nil {
		t.Fatal("cached groups escaped off mode")
	}
	if _, err := m.SaveGroup(ctx, a.ID, "Renamed"); err != nil {
		t.Fatal(err)
	}
	check(get(0, true), []string{b.Name, "Renamed"}, &one)
	if _, err := m.ReorderGroups(ctx, []string{a.ID, hidden.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	check(get(0, true), []string{"Renamed", b.Name}, &zero)
	if _, err := f.nodes.PatchMetadata(ctx, f.id, nodes.MetadataPatch{GroupID: json.RawMessage(`"` + b.ID + `"`)}); err != nil {
		t.Fatal(err)
	}
	check(get(0, true), []string{b.Name}, &zero)
	if err := m.DeleteGroup(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	check(get(0, true), []string{}, nil)
}

func TestPublicServiceGroupsInvalidateCachedPages(t *testing.T) {
	f := publicPageFixture(t, 1)
	ctx := t.Context()
	m := services.New(f.nodes.Database(), nil, func() time.Time { return f.now }, nil, nil, f.nodes.InvalidatePublicSafety)
	a, err := m.SaveGroup(ctx, "", "First")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.SaveGroup(ctx, "", "Second")
	if err != nil {
		t.Fatal(err)
	}
	name, kind, target, yes := "Health", "tcp", "localhost:443", true
	member := func(id string) json.RawMessage { return json.RawMessage(`"` + id + `"`) }
	first, err := m.Save(ctx, "", services.Patch{Name: &name, Type: &kind, Target: &target, Public: &yes, GroupID: member(a.ID)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Save(ctx, "", services.Patch{Name: &name, Type: &kind, Target: &target, Public: &yes, GroupID: member(b.ID)})
	if err != nil {
		t.Fatal(err)
	}
	check := func(names []string) {
		t.Helper()
		page, err := f.service.PublicPageWithGroups(ctx, f.nodes, 1, 0, time.Hour, func() time.Time { return f.now }, true, m)
		if err != nil || page.ServiceGroups == nil || !reflect.DeepEqual(*page.ServiceGroups, names) {
			t.Fatalf("cached service groups: %+v %v", page, err)
		}
	}
	check([]string{"First", "Second"})
	if _, err := m.SaveGroup(ctx, a.ID, "Renamed"); err != nil {
		t.Fatal(err)
	}
	check([]string{"Renamed", "Second"})
	if _, err := m.ReorderGroups(ctx, []string{b.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	check([]string{"Second", "Renamed"})
	if _, err := m.Save(ctx, first.ID, services.Patch{GroupID: member(b.ID)}); err != nil {
		t.Fatal(err)
	}
	check([]string{"Second"})
	if err := m.DeleteGroup(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	check([]string{})
}
