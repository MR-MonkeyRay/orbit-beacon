package nodes_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/validation"
)

func TestPublicSettingsRemainIndependentAndValidateAtomically(t *testing.T) {
	s, db := newNodes(t, 10)
	ctx := audit.WithActor(context.Background(), "test-admin")
	n := pending(t, s, "private-203.0.113.7")
	got, err := s.GetPublicSettings(ctx, n.ID)
	if err != nil || got != (nodes.PublicSettings{}) {
		t.Fatalf("default: %+v %v", got, err)
	}
	var patch nodes.PublicSettingsPatch
	if err := json.Unmarshal([]byte(`{"enabled":true,"name":" 东京节点 ","region":" 东京 ","provider":" Example "}`), &patch); err != nil {
		t.Fatal(err)
	}
	got, err = s.PatchPublicSettings(ctx, n.ID, patch)
	if err != nil || !got.Enabled || got.Name != "东京节点" || got.Region != "东京" || got.Provider != "Example" {
		t.Fatalf("publish: %+v %v", got, err)
	}
	original := got
	for _, name := range []string{"", strings.Repeat("中", 86), "control\x00", string([]byte{0xff})} {
		_, err := s.PatchPublicSettings(ctx, n.ID, nodes.PublicSettingsPatch{Name: &name})
		var fields *validation.Error
		if !errors.As(err, &fields) || fields.Fields["name"] == "" {
			t.Fatalf("invalid name %q: %v", name, err)
		}
		after, err := s.GetPublicSettings(ctx, n.ID)
		if err != nil || after != original {
			t.Fatalf("invalid patch changed settings: %+v %v", after, err)
		}
	}
	// Decode a fresh patch: omitted/null fields preserve the stored value.
	patch = nodes.PublicSettingsPatch{}
	_ = json.Unmarshal([]byte(`{"name":null,"region":"","enabled":false}`), &patch)
	got, err = s.PatchPublicSettings(ctx, n.ID, patch)
	if err != nil || got.Enabled || got.Name != original.Name || got.Region != "" {
		t.Fatalf("clear/preserve: %+v %v", got, err)
	}
	internal, err := s.Get(ctx, n.ID)
	if err != nil || internal.Name != n.Name {
		t.Fatalf("private metadata changed: %+v %v", internal, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='node.public_settings.patch' AND target_id=?`, n.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("audit count %d: %v", count, err)
	}
	if _, err := s.GetPublicSettings(ctx, "00000000-0000-4000-8000-000000009999"); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestPublicVisibilityAndMalformedSettings(t *testing.T) {
	s, db := newNodes(t, 20)
	ctx := context.Background()
	for i, settings := range []string{`{"enabled":true,"name":"Public"}`, `{}`, `{"enabled":1,"name":"wrong type"}`, `{"enabled":true,"name":""}`, `not json`, `{"enabled":true,"name":"bad\nname"}`} {
		n := pending(t, s, "private")
		if _, err := db.Exec(`UPDATE nodes SET public_settings=?, status='unknown', token_hash=randomblob(32), first_enrolled_at=created_at WHERE id=?`, settings, n.ID); err != nil {
			t.Fatal(err)
		}
		record, err := nodes.ReadPublicRecord(ctx, db, n.ID)
		if i == 0 {
			if err != nil || record.Settings.Name != "Public" {
				t.Fatalf("visible record: %+v %v", record, err)
			}
		} else if !errors.Is(err, nodes.ErrNotFound) {
			t.Fatalf("invalid settings %q exposed: %+v %v", settings, record, err)
		}
	}
	ids, more, err := s.ListPublicIDs(ctx, 1, 0)
	if err != nil || len(ids) != 1 || more {
		t.Fatalf("public list: %v %v %v", ids, more, err)
	}
	if err := s.SetDisabled(ctx, ids[0], true); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.ReadPublicRecord(ctx, db, ids[0]); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("disabled exposed: %v", err)
	}
	ids, _, err = s.ListPublicIDs(ctx, 100, 0)
	if err != nil || len(ids) != 0 {
		t.Fatalf("invalid or disabled exposed: %v %v", ids, err)
	}
	for _, args := range [][2]int{{0, 0}, {101, 0}, {1, -1}} {
		if _, _, err := s.ListPublicIDs(ctx, args[0], args[1]); !errors.Is(err, nodes.ErrInvalidInput) {
			t.Fatalf("pagination %v: %v", args, err)
		}
	}
}
