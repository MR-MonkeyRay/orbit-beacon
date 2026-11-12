package nodes_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"orbitbeacon/internal/server/nodes"
)

func TestPublicSettingsDefaultsCanonicalAndAtomic(t *testing.T) {
	s, _ := newNodes(t, 10)
	n := pending(t, s, "own name")
	ctx := context.Background()
	settings, err := s.GetPublicSettings(ctx, n.ID)
	if err != nil || settings.Enabled || !reflect.DeepEqual(settings.Fields, nodes.DefaultPublicFields) {
		t.Fatalf("default %+v %v", settings, err)
	}
	var patch nodes.PublicSettingsPatch
	if err := json.Unmarshal([]byte(`{"enabled":true,"fields":["price","region","cpu"]}`), &patch); err != nil {
		t.Fatal(err)
	}
	got, err := s.PatchPublicSettings(ctx, n.ID, patch)
	if err != nil || !got.Enabled || !reflect.DeepEqual(got.Fields, []string{"region", "cpu", "price"}) {
		t.Fatalf("canonical %+v %v", got, err)
	}
	for _, fields := range [][]string{{"nope"}, {"cpu", "cpu"}} {
		if _, err := s.PatchPublicSettings(ctx, n.ID, nodes.PublicSettingsPatch{Fields: &fields}); err == nil {
			t.Fatal("invalid accepted")
		}
		after, err := s.GetPublicSettings(ctx, n.ID)
		if err != nil || !reflect.DeepEqual(after, got) {
			t.Fatal("not atomic")
		}
	}
	for _, raw := range []string{`{"name":"alias"}`, `{"fields":["` + string([]byte{255}) + `"]}`} {
		if json.Unmarshal([]byte(raw), &patch) == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	empty := []string{}
	got, err = s.PatchPublicSettings(ctx, n.ID, nodes.PublicSettingsPatch{Fields: &empty})
	if err != nil || got.Fields == nil || len(got.Fields) != 0 {
		t.Fatal("empty fields not preserved")
	}
}
