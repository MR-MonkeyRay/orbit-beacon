package alerts

import (
	"context"
	"testing"

	"orbitbeacon/internal/timekey"
)

func TestChannelRoutingPreservesPendingAndOpenEpisodes(t *testing.T) {
	f := newFixture(t, 60)
	channel := "00000000-0000-4000-8000-000000000099"
	f.exec(t, `INSERT INTO notification_channels(id,name,kind,enabled,config_json,created_at,updated_at) VALUES(?,'hook','webhook',1,'{}',?,?)`, channel, timekey.Format(f.now), timekey.Format(f.now))
	start := f.now
	f.tick(t)
	f.advance(30)
	f.value(90)
	if _, e := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"channel_ids":["`+channel+`"]}`)); e != nil {
		t.Fatal(e)
	}
	f.tick(t)
	pending := f.e.pending[pair{f.rule.ID, f.node}]
	if !pending.since.Equal(start) {
		t.Fatal("routing reset pending timer")
	}
	f.advance(30)
	f.value(90)
	f.tick(t)
	events := f.events(t, "firing")
	if len(events) != 1 || !events[0].StartedAt.Equal(start) {
		t.Fatal("pending did not fire at original deadline")
	}
	id := events[0].ID
	if _, e := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"channel_ids":[]}`)); e != nil {
		t.Fatal(e)
	}
	f.advance(1)
	f.value(90)
	f.tick(t)
	events = f.events(t, "all")
	if len(events) != 1 || events[0].ID != id || events[0].State != "firing" {
		t.Fatal("routing resolved or replaced open episode")
	}
	f.e = NewEvaluator(f.s, f.latest, EvaluatorOptions{})
	f.tick(t)
	events = f.events(t, "all")
	if len(events) != 1 || events[0].ID != id || events[0].ResolvedAt != nil {
		t.Fatal("restart changed routed episode")
	}
	// 迁移前的快照不含 channel_ids；null 与空的路由等价。
	legacy := f.rule
	legacy.ChannelIDs = nil
	current := legacy
	current.ChannelIDs = []string{channel}
	if definition(legacy) != definition(current) {
		t.Fatal("legacy definition changed")
	}
}
