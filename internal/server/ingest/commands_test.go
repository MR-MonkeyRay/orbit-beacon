package ingest

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/commands"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func createTestCommand(t *testing.T, f *coalesceFixture, kind string) commands.Record {
	t.Helper()
	r, err := commands.New(f.db, func() time.Time { return f.now }).Create(t.Context(), f.id, "operator", commands.CreateRequest{Kind: kind, Confirm: "coalesce"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func commandStateFor(t *testing.T, f *coalesceFixture, id string) string {
	t.Helper()
	var state string
	if err := f.db.QueryRow(`SELECT state FROM agent_commands WHERE id=?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
func TestCommandsPreserveCoalescingAndRedeliverUntilResult(t *testing.T) {
	f := newCoalesceFixture(t, time.Hour)
	writes := f.writes(t)
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	response, err := acceptReport(t.Context(), f.service, coalesceToken, f.report)
	if err != nil || response.Command != nil || f.writes(t) != writes {
		t.Fatalf("idle writes changed: %+v %v", response, err)
	}
	c := createTestCommand(t, f, "restart")
	for range 2 {
		f.now = f.now.Add(time.Second)
		f.report.Seq++
		response, err = acceptReport(t.Context(), f.service, coalesceToken, f.report)
		if err != nil || response.Command == nil || response.Command.ID != c.ID {
			t.Fatalf("delivery: %+v %v", response, err)
		}
	}
	if f.writes(t) != writes+2 || commandStateFor(t, f, c.ID) != "delivered" {
		t.Fatal("command did not force durability")
	}
	f.report.CommandResults = []v1.CommandResult{{ID: c.ID, Status: "succeeded", Code: "ok"}}
	for range 2 {
		f.now = f.now.Add(time.Second)
		f.report.Seq++
		response, err = acceptReport(t.Context(), f.service, coalesceToken, f.report)
		if err != nil || response.Command != nil {
			t.Fatalf("result: %+v %v", response, err)
		}
	}
	if commandStateFor(t, f, c.ID) != "succeeded" {
		t.Fatal("result missing")
	}
	var finished string
	if err := f.db.QueryRow(`SELECT finished_at FROM agent_commands WHERE id=?`, c.ID).Scan(&finished); err != nil {
		t.Fatal(err)
	}
	if finished != timekey.Format(f.now.Add(-time.Second)) {
		t.Fatal("duplicate changed terminal timestamp")
	}
}
func TestCommandsExpireAndCancelWrongEpoch(t *testing.T) {
	for _, kind := range []string{"expiry", "epoch"} {
		t.Run(kind, func(t *testing.T) {
			f := newCoalesceFixture(t, time.Hour)
			c := createTestCommand(t, f, "restart")
			want := "expired"
			if kind == "expiry" {
				f.now = c.ExpiresAt
			} else {
				want = "canceled"
				f.now = f.now.Add(time.Second)
				if _, err := f.db.Exec(`UPDATE nodes SET credential_epoch=credential_epoch+1 WHERE id=?`, f.id); err != nil {
					t.Fatal(err)
				}
			}
			f.report.Seq++
			r, err := acceptReport(t.Context(), f.service, coalesceToken, f.report)
			if err != nil || r.Command != nil || commandStateFor(t, f, c.ID) != want {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}
func TestCommandsRevocationDisableAndReenrollmentCancel(t *testing.T) {
	for _, mutation := range []string{"revoke", "disable", "reenroll", "rotation"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCoalesceFixture(t, time.Hour)
			c := createTestCommand(t, f, "restart")
			admission, err := f.service.AdmitToken(t.Context(), coalesceToken)
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "disable" {
				err = f.nodes.SetDisabled(t.Context(), f.id, true)
			} else if mutation != "rotation" {
				err = f.nodes.RevokeCredential(t.Context(), f.id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "reenroll" || mutation == "rotation" {
				es := enrollment.NewService(f.db, enrollment.Options{})
				ticket, err := es.CreateSingle(t.Context(), f.id)
				if err != nil {
					t.Fatal(err)
				}
				req := v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "new", NodeToken: coalesceToken}
				if mutation == "rotation" {
					req.PreviousNodeToken = coalesceToken
					req.NodeToken = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
				}
				if _, err := es.Enroll(t.Context(), ticket.Token, req); err != nil {
					t.Fatal(err)
				}
			}
			if commandStateFor(t, f, c.ID) != "canceled" {
				t.Fatal("active survived identity change")
			}
			f.report.Seq++
			f.report.CommandResults = []v1.CommandResult{{ID: c.ID, Status: "succeeded", Code: "ok"}}
			if _, err := f.service.AcceptAdmitted(t.Context(), admission, f.report); err == nil {
				t.Fatal("stale admission accepted")
			}
			if commandStateFor(t, f, c.ID) != "canceled" {
				t.Fatal("stale admission changed terminal state")
			}
		})
	}
}

// Hold the writer gate after acceptance SELECT, then change the stored identity.
// This exercises the transaction recheck rather than only initial authentication.
func TestCommandWriteTransactionFencePrecedesAllCommandChanges(t *testing.T) {
	for _, mutation := range []string{"token_hash=NULL", "credential_epoch=credential_epoch+1", "run_generation='2'", "report_session_id='changed'", "status='disabled'"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCoalesceFixture(t, time.Hour)
			c := createTestCommand(t, f, "restart")
			f.report.Seq++
			f.report.CommandResults = []v1.CommandResult{{ID: c.ID, Status: "succeeded", Code: "ok"}}
			f.service.writer <- struct{}{}
			done := make(chan error, 1)
			go func() { _, err := acceptReport(context.Background(), f.service, coalesceToken, f.report); done <- err }()
			deadline := time.After(3 * time.Second)
			for len(f.service.writerQueue) == 0 {
				select {
				case <-deadline:
					<-f.service.writer
					t.Fatal("report did not reach writer gate")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			if _, err := f.db.Exec(`UPDATE nodes SET `+mutation+` WHERE id=?`, f.id); err != nil {
				<-f.service.writer
				t.Fatal(err)
			}
			<-f.service.writer
			if err := <-done; err == nil {
				t.Fatal("fence failure accepted")
			}
			if commandStateFor(t, f, c.ID) != "pending" {
				t.Fatal("fenced report mutated command")
			}
		})
	}
}
func TestCommandResultsCannotRewriteOtherNodeOrEpoch(t *testing.T) {
	f := newCoalesceFixture(t, time.Hour)
	c := createTestCommand(t, f, "restart")
	other, err := f.nodes.CreatePending(t.Context(), nodes.CreateRequest{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{fmt.Sprintf("node_id='%s'", other.ID), "credential_epoch=credential_epoch+1"} {
		tx, err := f.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`UPDATE agent_commands SET `+change+` WHERE id=?`, c.ID); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		// Results are ignored on both mismatches. Wrong epoch may then cancel the active row.
		_, err = commands.Exchange(t.Context(), tx, f.id, 0, "1", []v1.CommandResult{{ID: c.ID, Status: "succeeded", Code: "ok"}}, f.now)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		var state string
		if err := tx.QueryRow(`SELECT state FROM agent_commands WHERE id=?`, c.ID).Scan(&state); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if state == "succeeded" {
			t.Fatal("foreign result accepted")
		}
		tx.Rollback()
	}
}
func TestStopResultAndNewRunClearsStopped(t *testing.T) {
	f := newCoalesceFixture(t, time.Hour)
	c := createTestCommand(t, f, "stop")
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	if _, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.CommandResults = []v1.CommandResult{{ID: c.ID, Status: "succeeded", Code: "ok"}}
	if _, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	n, err := f.nodes.Get(t.Context(), f.id)
	if err != nil || !n.Stopped {
		t.Fatalf("not stopped: %+v %v", n, err)
	}
	f.now = f.now.Add(time.Second)
	if _, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	n, err = f.nodes.Get(t.Context(), f.id)
	if err != nil || !n.Stopped {
		t.Fatalf("lost-response retry cleared stopped: %+v %v", n, err)
	}
	f.now = f.now.Add(time.Second)
	f.report.RunGeneration++
	f.report.SessionID = "next"
	f.report.Seq = 1
	f.report.CommandResults = nil
	if _, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	n, err = f.nodes.Get(t.Context(), f.id)
	if err != nil || n.Stopped {
		t.Fatalf("stopped not cleared: %+v %v", n, err)
	}
}
func TestCommandResultTransactionRollsBackOnAuditFailure(t *testing.T) {
	f := newCoalesceFixture(t, time.Hour)
	c := createTestCommand(t, f, "stop")
	before := f.writes(t)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_command_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'test'); END`); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.CommandResults = []v1.CommandResult{{ID: c.ID, Status: "succeeded", Code: "ok"}}
	if _, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err == nil {
		t.Fatal("audit failure accepted")
	}
	if f.writes(t) != before || commandStateFor(t, f, c.ID) != "pending" {
		t.Fatal("partial transaction")
	}
}

func TestStopResultOnDuplicateSnapshotMarksStopped(t *testing.T) {
	f := newCoalesceFixture(t, time.Hour)
	c := createTestCommand(t, f, "stop")
	if _, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	f.report.CommandResults = []v1.CommandResult{{ID: c.ID, Status: "succeeded", Code: "ok"}}
	if _, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	n, err := f.nodes.Get(t.Context(), f.id)
	if err != nil || !n.Stopped {
		t.Fatalf("duplicate result not applied: %+v %v", n, err)
	}
}

func TestStoppedOnlyDescribesTheRunThatReceivedTheCommand(t *testing.T) {
	deliver := func(t *testing.T, f *coalesceFixture) v1.CommandResult {
		t.Helper()
		c := createTestCommand(t, f, "stop")
		f.now = f.now.Add(time.Second)
		f.report.Seq++
		response, err := acceptReport(t.Context(), f.service, coalesceToken, f.report)
		if err != nil || response.Command == nil {
			t.Fatalf("delivery: %+v %v", response, err)
		}
		return v1.CommandResult{ID: c.ID, Status: "succeeded", Code: "ok"}
	}
	nextRun := func(f *coalesceFixture) {
		f.now = f.now.Add(time.Second)
		f.report.RunGeneration++
		f.report.SessionID = "next"
		f.report.Seq = 1
	}
	stopped := func(t *testing.T, f *coalesceFixture) bool {
		t.Helper()
		n, err := f.nodes.Get(t.Context(), f.id)
		if err != nil {
			t.Fatal(err)
		}
		return n.Stopped
	}
	t.Run("result recovered by a new process", func(t *testing.T) {
		f := newCoalesceFixture(t, time.Hour)
		result := deliver(t, f)
		nextRun(f)
		f.report.CommandResults = []v1.CommandResult{result}
		if _, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err != nil {
			t.Fatal(err)
		}
		if commandStateFor(t, f, result.ID) != "succeeded" || stopped(t, f) {
			t.Fatal("a recovered result marked the running process as stopped")
		}
	})
	t.Run("redelivered to a new process", func(t *testing.T) {
		f := newCoalesceFixture(t, time.Hour)
		result := deliver(t, f)
		nextRun(f)
		if response, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err != nil || response.Command == nil {
			t.Fatalf("redelivery: %+v %v", response, err)
		}
		f.now = f.now.Add(time.Second)
		f.report.Seq++
		f.report.CommandResults = []v1.CommandResult{result}
		if _, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err != nil {
			t.Fatal(err)
		}
		if !stopped(t, f) {
			t.Fatal("the process that executed the stop is not stopped")
		}
	})
	t.Run("retry after server restart", func(t *testing.T) {
		f := newCoalesceFixture(t, time.Hour)
		result := deliver(t, f)
		f.now = f.now.Add(time.Second)
		f.report.Seq++
		f.report.CommandResults = []v1.CommandResult{result}
		if _, err := acceptReport(t.Context(), f.service, coalesceToken, f.report); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(time.Second)
		restarted := NewService(f.db, Options{Now: func() time.Time { return f.now }})
		if _, err := acceptReport(t.Context(), restarted, coalesceToken, f.report); err != nil {
			t.Fatal(err)
		}
		if !stopped(t, f) {
			t.Fatal("a lost-response retry cleared stopped")
		}
	})
}
