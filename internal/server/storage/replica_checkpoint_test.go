package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestReplicaSeedCheckpointRequiresMatchingControlProof(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	ctx := context.Background()
	replicaWrite(t, source, "INSERT INTO settings VALUES('copied','complete')")
	p, err := position(ctx, source.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = target.current.writer.ExecContext(ctx, "INSERT INTO settings VALUES('copied','complete')"); err != nil {
		t.Fatal(err)
	}
	reset := func() {
		t.Helper()
		if _, err := target.current.writer.ExecContext(ctx, "UPDATE replica_state SET epoch_uuid=?,head_seq=?,head_tx_uuid=?,retained_from_seq=?,journal_bytes=0,capture_mode='off',ready=1 WHERE id=1", p.Epoch, p.Seq, p.ID, p.Seq+1); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name   string
		proof  replicaProof
		mutate func()
	}{
		{name: "missing_control"},
		{name: "wrong_uuid", proof: replicaProof{Checkpoint: &replicaPosition{Epoch: p.Epoch, Seq: p.Seq, ID: uuid.NewString()}, Ready: true}},
		{name: "wrong_epoch", proof: replicaProof{Checkpoint: &replicaPosition{Epoch: "other", Seq: p.Seq, ID: p.ID}, Ready: true}},
		{name: "wrong_position", proof: replicaProof{Checkpoint: &replicaPosition{Epoch: p.Epoch, Seq: p.Seq + 1, ID: p.ID}, Ready: true}},
		{name: "wrong_instance", proof: replicaProof{Checkpoint: (&memberRecord{Checkpoint: &replicaCheckpoint{Instance: "other", Position: p}}).checkpointFor("1"), Ready: true}},
		{name: "sql_unready", proof: replicaProof{Checkpoint: &p, Ready: true}, mutate: func() {
			if _, err := target.current.writer.ExecContext(ctx, "UPDATE replica_state SET ready=0"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid_uuid", proof: replicaProof{Rebuilding: true}, mutate: func() {
			if _, err := target.current.writer.ExecContext(ctx, "UPDATE replica_state SET head_tx_uuid='invalid'"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing_epoch", proof: replicaProof{Rebuilding: true}, mutate: func() {
			if _, err := target.current.writer.ExecContext(ctx, "UPDATE replica_state SET epoch_uuid='missing'"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset()
			if tc.mutate != nil {
				tc.mutate()
			}
			if err := installReplica(ctx, target, "resource", "1", p.Epoch, tc.proof); !errors.Is(err, ErrIdentityMismatch) {
				t.Fatal("invalid checkpoint accepted", err)
			}
		})
	}
	reset()
	if err = installReplica(ctx, target, "resource", "1", p.Epoch, replicaProof{Checkpoint: &p, Ready: true}); err != nil {
		t.Fatal("registered complete seed rejected", err)
	}
	reset()
	if _, err = target.current.writer.ExecContext(ctx, "UPDATE replica_state SET ready=0"); err != nil {
		t.Fatal(err)
	}
	if err = installReplica(ctx, target, "resource", "1", p.Epoch, replicaProof{Rebuilding: true}); err != nil {
		t.Fatal("registered interrupted inactive seed cannot restart", err)
	}
	var ready bool
	if err = target.current.reader.QueryRowContext(ctx, "SELECT ready FROM replica_state").Scan(&ready); err != nil || ready {
		t.Fatal("metadata validation restored incomplete copy readiness", ready, err)
	}
}
