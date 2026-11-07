package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"orbitbeacon/internal/agent/identity"
	v1 "orbitbeacon/internal/protocol/v1"
)

func commandTestStore(t *testing.T) *commandStore {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := openCommandStore(filepath.Join(dir, "commands.json"), slog.New(slog.DiscardHandler), "")
	if s.blocked {
		t.Fatal("blocked")
	}
	return s
}
func testCommand(kind string) v1.Command {
	return v1.Command{ID: "00000000-0000-4000-8000-000000000001", Kind: kind, ExpiresAt: time.Date(2030, 1, 1, 0, 5, 0, 0, time.UTC)}
}
func TestCommandsRejectAndDeduplicate(t *testing.T) {
	for _, test := range []struct {
		name, kind, code  string
		disabled, expired bool
	}{
		{name: "disabled", kind: "restart", code: "disabled_locally", disabled: true},
		{name: "upgrade reserve", kind: "upgrade", code: "expired"},
		{name: "expired", kind: "stop", code: "expired", expired: true},
		{name: "unknown", kind: "shell"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := commandTestStore(t)
			c := testCommand(test.kind)
			if c.Kind == "upgrade" {
				c.TargetVersion = "1.2.3"
			}
			now := c.ExpiresAt.Add(-time.Minute)
			if test.expired {
				now = c.ExpiresAt
			}
			settings := v1.DefaultRuntimeSettings()
			if test.disabled {
				settings.RemoteCommands = nil
			}
			if action := s.handle(c, now, settings); action != "" {
				t.Fatal(action)
			}
			if test.code == "" {
				if len(s.results()) != 0 {
					t.Fatal("invalid command acknowledged")
				}
				return
			}
			if got := s.results(); len(got) != 1 || got[0].Code != test.code {
				t.Fatal(got)
			}
			s.handle(c, now, settings)
			if len(s.results()) != 1 {
				t.Fatal("duplicate")
			}
		})
	}
}
func TestCommandCrashRecoveryAndResponseLoss(t *testing.T) {
	for _, kind := range []string{"restart", "stop"} {
		t.Run(kind, func(t *testing.T) {
			s := commandTestStore(t)
			c := testCommand(kind)
			now := c.ExpiresAt.Add(-time.Minute)
			if got := s.handle(c, now, v1.DefaultRuntimeSettings()); got != kind {
				t.Fatal(got)
			}
			if got := s.handle(c, now, v1.DefaultRuntimeSettings()); got != "" {
				t.Fatal("executed twice")
			}
			recovered := openCommandStore(s.path, s.logger, "")
			if recovered.blocked {
				t.Fatal("recovery blocked")
			}
			if got := recovered.results(); len(got) != 1 || got[0].Status != "succeeded" || got[0].Code != "ok" {
				t.Fatal(got)
			}
			if got := recovered.handle(c, now, v1.DefaultRuntimeSettings()); got != "" {
				t.Fatal("replayed after crash")
			}
			// Only a successful response acknowledges a result; reopening retains it.
			recovered = openCommandStore(s.path, s.logger, "")
			results := recovered.results()
			if len(results) != 1 {
				t.Fatal(results)
			}
			recovered.acknowledge(results)
			recovered = openCommandStore(s.path, s.logger, "")
			if len(recovered.results()) != 0 {
				t.Fatal("ack lost")
			}
			if recovered.handle(c, now, v1.DefaultRuntimeSettings()) != "" {
				t.Fatal("ack removed dedup ID")
			}
		})
	}
}
func TestStopInterruptedAtExecutionBarrier(t *testing.T) {
	s := commandTestStore(t)
	c := testCommand("stop")
	writes := 0
	persist := s.persist
	s.persist = func(state commandState) error {
		writes++
		if writes == 2 {
			return errors.New("crash before result persistence")
		}
		return persist(state)
	}
	if s.handle(c, c.ExpiresAt.Add(-time.Minute), v1.DefaultRuntimeSettings()) != "" {
		t.Fatal("executed despite write failure")
	}
	recovered := openCommandStore(s.path, s.logger, "")
	results := recovered.results()
	if recovered.blocked || len(results) != 1 || results[0].Status != "failed" || results[0].Code != "interrupted" {
		t.Fatal(results)
	}
	if recovered.handle(c, c.ExpiresAt.Add(-time.Minute), v1.DefaultRuntimeSettings()) != "" {
		t.Fatal("stop repeated")
	}
}
func TestCommandStateCorruptAndWriteFailureFailClosed(t *testing.T) {
	s := commandTestStore(t)
	s.persist = func(commandState) error { return errors.New("disk full") }
	c := testCommand("restart")
	if s.handle(c, c.ExpiresAt.Add(-time.Minute), v1.DefaultRuntimeSettings()) != "" || !s.blocked {
		t.Fatal("write failure executed")
	}
	if err := os.WriteFile(s.path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := openCommandStore(s.path, s.logger, "")
	if !corrupt.blocked || corrupt.handle(c, c.ExpiresAt.Add(-time.Minute), v1.DefaultRuntimeSettings()) != "" {
		t.Fatal("corruption executed")
	}
	data, _ := os.ReadFile(s.path)
	if string(data) != "invalid" {
		t.Fatal("corruption removed")
	}
}
func TestCommandHistorySaturationCannotReplay(t *testing.T) {
	s := commandTestStore(t)
	now := testCommand("stop").ExpiresAt.Add(-time.Minute)
	for i := range 33 {
		c := testCommand("stop")
		c.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		action := s.handle(c, now, v1.DefaultRuntimeSettings())
		if (i < 32) != (action == "stop") {
			t.Fatalf("%d: %s", i, action)
		}
		s.acknowledge(s.results())
	}
	c := testCommand("stop")
	c.ID = "00000000-0000-4000-8000-000000000000"
	if s.handle(c, now, v1.DefaultRuntimeSettings()) != "" {
		t.Fatal("old ID replay")
	}
	overflow := s.state.Pending
	if len(overflow) != 0 {
		t.Fatal("acknowledged capacity rejection retained")
	}
	c.ID = "00000000-0000-4000-8000-000000000099"
	s.handle(c, now, v1.DefaultRuntimeSettings())
	if got := s.results(); len(got) != 1 || got[0].Status != "rejected" || got[0].Code != "failed" {
		t.Fatal(got)
	}
	if len(s.state.Processed) != 32 {
		t.Fatal("history unbounded")
	}
}
func TestCommandSchedulerExitAndImmediateStopReport(t *testing.T) {
	for _, kind := range []string{"restart", "stop", "stop timeout", "stop identity conflict"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := commandTestStore(t)
				id, err := identity.Open(t.Context(), filepath.Join(filepath.Dir(store.path), "identity.json"), identity.Options{})
				if err != nil {
					t.Fatal(err)
				}
				commandKind := strings.Fields(kind)[0]
				c := testCommand(commandKind)
				calls := 0
				started := time.Now()
				s := NewScheduler(SchedulerOptions{Identity: id, Commands: store, RuntimeSettings: v1.DefaultRuntimeSettings, Collect: func(context.Context) (v1.Metrics, error) { return v1.Metrics{}, nil }, Report: func(ctx context.Context, r v1.Report) (v1.ReportResponse, error) {
					calls++
					if calls == 1 {
						return v1.ReportResponse{Command: &c, ServerTime: c.ExpiresAt.Add(-time.Minute)}, nil
					}
					if commandKind != "stop" || len(r.CommandResults) != 1 || r.CommandResults[0].Code != "ok" {
						t.Fatalf("bad report: %+v", r.CommandResults)
					}
					if kind == "stop timeout" {
						<-ctx.Done()
						return v1.ReportResponse{}, ctx.Err()
					}
					if kind == "stop identity conflict" {
						return v1.ReportResponse{}, &HTTPError{StatusCode: 409, Code: "identity_conflict"}
					}
					return v1.ReportResponse{}, nil
				}})
				err = s.Run(t.Context())
				var exit *ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				want := 0
				if kind == "restart" {
					want = 75
				}
				if exit.Code != want {
					t.Fatal(exit)
				}
				if kind == "stop" && (calls != 2 || time.Since(started) > time.Second) {
					t.Fatalf("stop not immediate: %d %s", calls, time.Since(started))
				}
				if strings.HasPrefix(kind, "stop ") && time.Since(started) != 15*time.Second {
					t.Fatal(time.Since(started))
				}
			})
		})
	}
}

func TestCommandResultsSendOldestTwoAndOnlyAcknowledgeSent(t *testing.T) {
	s := commandTestStore(t)
	now := testCommand("stop").ExpiresAt.Add(-time.Minute)
	settings := v1.DefaultRuntimeSettings()
	settings.RemoteCommands = nil
	for i := range 3 {
		c := testCommand("restart")
		c.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		s.handle(c, now, settings)
	}
	batch := s.results()
	if len(batch) != 2 || batch[0].ID != "00000000-0000-4000-8000-000000000000" || batch[1].ID != "00000000-0000-4000-8000-000000000001" {
		t.Fatal(batch)
	}
	// A response is lost: reopening must retain all three pending results.
	s = openCommandStore(s.path, s.logger, "")
	if len(s.state.Pending) != 3 {
		t.Fatal(s.state.Pending)
	}
	s.acknowledge(batch)
	s = openCommandStore(s.path, s.logger, "")
	remaining := s.results()
	if len(remaining) != 1 || remaining[0].ID != "00000000-0000-4000-8000-000000000002" {
		t.Fatal(remaining)
	}
}

func TestStopFlushesOldResultsBeforeOwnResultAndLostResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := commandTestStore(t)
		settings := v1.DefaultRuntimeSettings()
		disabled := settings
		disabled.RemoteCommands = nil
		c := testCommand("stop")
		now := c.ExpiresAt.Add(-time.Minute)
		for i := range 5 {
			old := testCommand("restart")
			old.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+2)
			store.handle(old, now, disabled)
		}
		id, err := identity.Open(t.Context(), filepath.Join(filepath.Dir(store.path), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		calls, collections, ownResults := 0, 0, 0
		var flushedSeq uint64
		var scheduler *Scheduler
		scheduler = NewScheduler(SchedulerOptions{Identity: id, Commands: store, RuntimeSettings: func() v1.RuntimeSettings { return settings }, RandomFloat: func() float64 { return 0.5 }, Collect: func(context.Context) (v1.Metrics, error) { collections++; return v1.Metrics{}, nil }, Report: func(ctx context.Context, report v1.Report) (v1.ReportResponse, error) {
			calls++
			if len(report.CommandResults) > 2 {
				t.Fatal("oversized result batch")
			}
			if calls == 1 {
				return v1.ReportResponse{Command: &c, ServerTime: now}, nil
			}
			if flushedSeq == 0 {
				flushedSeq = report.Seq
			}
			if report.Seq != flushedSeq {
				t.Fatal("stop retry recollected and advanced last_seen")
			}
			for _, r := range report.CommandResults {
				if r.ID == c.ID {
					ownResults++
					if ownResults == 1 {
						// A collection already in progress when stop arrived publishes
						// after the first result request; retry must keep its snapshot.
						scheduler.mu.Lock()
						scheduler.latest.Seq++
						scheduler.mu.Unlock()
						time.Sleep(11 * time.Second)
						return v1.ReportResponse{}, errors.New("response lost")
					}
				}
			}
			return v1.ReportResponse{ServerTime: now}, nil
		}})
		err = scheduler.Run(t.Context())
		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code != 0 {
			t.Fatal(err)
		}
		if collections != 2 || ownResults != 2 || len(store.state.Pending) != 0 {
			t.Fatalf("collections=%d own=%d pending=%+v", collections, ownResults, store.state.Pending)
		}
	})
}
func TestCommandAcknowledgementFailureDisablesNextExecution(t *testing.T) {
	s := commandTestStore(t)
	c := testCommand("restart")
	settings := v1.DefaultRuntimeSettings()
	settings.RemoteCommands = nil
	s.handle(c, c.ExpiresAt.Add(-time.Minute), settings)
	s.persist = func(commandState) error { return errors.New("sync failed") }
	s.acknowledge(s.results())
	next := c
	next.ID = "00000000-0000-4000-8000-000000000002"
	if s.handle(next, c.ExpiresAt.Add(-time.Minute), v1.DefaultRuntimeSettings()) != "" || !s.blocked {
		t.Fatal("ack persistence failure allowed execution")
	}
	recovered := openCommandStore(s.path, s.logger, "")
	if len(recovered.results()) != 1 {
		t.Fatal("unpersisted acknowledgement removed pending result")
	}
}

func TestCommandsOnlyEvictExpiredAcknowledgedIDs(t *testing.T) {
	s := commandTestStore(t)
	settings := v1.DefaultRuntimeSettings()
	settings.RemoteCommands = nil
	now := testCommand("restart").ExpiresAt.Add(-time.Minute)
	for i := range commandHistoryLimit {
		c := testCommand("restart")
		c.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		s.handle(c, now, settings)
	}
	next := testCommand("restart")
	next.ID = "00000000-0000-4000-8000-999999999999"
	next.ExpiresAt = next.ExpiresAt.Add(time.Hour)
	// All history expired, but results are still pending: no eviction is safe.
	s.handle(next, now.Add(2*time.Minute), v1.DefaultRuntimeSettings())
	last := s.state.Pending[len(s.state.Pending)-1]
	if last.ID != next.ID || last.Status != "rejected" || last.Code != "failed" {
		t.Fatal(last)
	}
	for len(s.state.Pending) > 0 {
		s.acknowledge(s.results())
	}
	if got := s.handle(next, now.Add(2*time.Minute), v1.DefaultRuntimeSettings()); got != "restart" {
		t.Fatal(got)
	}
	if len(s.state.Processed) != 32 {
		t.Fatal(len(s.state.Processed))
	}
}

func TestCommandExpiringDuringTheRequestIsNotExecuted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := commandTestStore(t)
		id, err := identity.Open(t.Context(), filepath.Join(filepath.Dir(store.path), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		c := testCommand(v1.RemoteCommandRestart)
		calls := 0
		s := NewScheduler(SchedulerOptions{Identity: id, Commands: store, RuntimeSettings: v1.DefaultRuntimeSettings, Collect: func(context.Context) (v1.Metrics, error) { return v1.Metrics{}, nil }, Report: func(context.Context, v1.Report) (v1.ReportResponse, error) {
			calls++
			if calls > 1 {
				return v1.ReportResponse{}, nil
			}
			// 响应生成时指令尚未过期，到达 Agent 时已过期。
			time.Sleep(2 * time.Second)
			return v1.ReportResponse{Command: &c, ServerTime: c.ExpiresAt.Add(-time.Second)}, nil
		}})
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		if err := s.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired command acted: %v", err)
		}
		if len(store.state.Processed) != 1 || store.state.Processed[0].Code != "expired" {
			t.Fatalf("processed=%+v", store.state.Processed)
		}
	})
}

func TestReloadedLocalSwitchBlocksNextCommand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := commandTestStore(t)
		id, err := identity.Open(t.Context(), filepath.Join(filepath.Dir(store.path), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		c := testCommand(v1.RemoteCommandRestart)
		settings := v1.DefaultRuntimeSettings()
		reload := make(chan struct{}, 1)
		var collected atomic.Int64
		s := NewScheduler(SchedulerOptions{Identity: id, Commands: store, Reload: reload,
			RuntimeSettings: func() v1.RuntimeSettings { return settings },
			ReloadConfig: func(context.Context) error {
				settings.RemoteCommands = []string{}
				return nil
			},
			Collect: func(context.Context) (v1.Metrics, error) {
				if collected.Add(1) == 1 {
					reload <- struct{}{}
				}
				return v1.Metrics{}, nil
			},
			Report: func(_ context.Context, r v1.Report) (v1.ReportResponse, error) {
				if r.Runtime.AllowsCommand(v1.RemoteCommandRestart) {
					return v1.ReportResponse{}, nil
				}
				return v1.ReportResponse{Command: &c, ServerTime: c.ExpiresAt.Add(-time.Minute)}, nil
			}})
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
		defer cancel()
		if err := s.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("locally disabled command acted: %v", err)
		}
		if len(store.state.Processed) != 1 || store.state.Processed[0].Code != "disabled_locally" {
			t.Fatalf("processed=%+v", store.state.Processed)
		}
	})
}
