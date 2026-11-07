package alerts

import (
	"context"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
	"testing"
	"time"
)

func TestStoppedSuppressesOfflineAndResolvesExistingEvent(t *testing.T) {
	f := newFixture(t, 0)
	rule, err := f.s.Save(t.Context(), f.rule.ID, patch(t, `{"metric":"offline","threshold":null,"comparator":null,"for_seconds":180}`))
	if err != nil {
		t.Fatal(err)
	}
	f.rule = rule
	f.latest.err = nodes.ErrNotFound
	f.advance(180)
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("offline did not fire")
	}
	f.exec(t, `INSERT INTO agent_commands(id,node_id,credential_epoch,kind,state,delivered_generation,requested_by,created_at,expires_at,finished_at) VALUES('stop',?,0,'stop','succeeded','0','operator',?,?,?)`, f.node, timekey.Format(f.now), timekey.Format(f.now.Add(time.Minute)), timekey.Format(f.now))
	f.tick(t)
	if len(f.events(t, "firing")) != 0 || len(f.events(t, "resolved")) != 1 {
		t.Fatal("stopped offline not resolved")
	}
	f.advance(600)
	f.tick(t)
	if len(f.events(t, "firing")) != 0 {
		t.Fatal("stopped fired offline")
	}
	f.exec(t, `UPDATE nodes SET run_generation='1',last_seen=? WHERE id=?`, timekey.Format(f.now), f.node)
	n, err := nodes.NewService(f.db, nodes.Options{}).Get(context.Background(), f.node)
	if err != nil || n.Stopped {
		t.Fatalf("new report failed to clear: %+v %v", n, err)
	}
	f.advance(180)
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("offline did not resume")
	}
}

func TestOfflineFireRechecksStoppedInsideWriteTransaction(t *testing.T) {
	f := newFixture(t, 0)
	rule, err := f.s.Save(t.Context(), f.rule.ID, patch(t, `{"metric":"offline","threshold":null,"comparator":null,"for_seconds":180}`))
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := f.e.readNodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.advance(180)
	f.exec(t, `INSERT INTO agent_commands(id,node_id,credential_epoch,kind,state,delivered_generation,requested_by,created_at,expires_at,finished_at) VALUES('stop',?,0,'stop','succeeded','0','operator',?,?,?)`, f.node, timekey.Format(f.now), timekey.Format(f.now.Add(time.Minute)), timekey.Format(f.now))
	if err := f.e.fire(t.Context(), rule, inventory[0], nil, f.now.Add(-180*time.Second), f.now); err != nil {
		t.Fatal(err)
	}
	if len(f.events(t, "firing")) != 0 {
		t.Fatal("stale evaluation fired after stop committed")
	}
}
