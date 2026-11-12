package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLIngestGenerationFailoverPreservesPrivatePending(t *testing.T) {
	for _, driver := range []string{"sqlite", "mysql"} {
		t.Run(driver, func(t *testing.T) {
			remote := storagetest.Start(t)
			proxy, primary := storagetest.ProxyEndpoint(t, remote.Endpoint("primary"))
			standby := remote.Endpoint("standby")
			if driver == "sqlite" {
				standby = config.DatabaseEndpoint{Driver: "sqlite"}
			}
			cfg := config.DatabaseConfig{Primary: primary, Standby: &standby, Failover: config.FailoverConfig{FailureTimeout: time.Second, MaxLossWindow: 10 * time.Second, RecoveryStable: time.Hour, ManualAfter: time.Hour}}
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			dbs, err := storage.OpenDatabases(ctx, cfg, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer dbs.Close()
			now := time.Now().UTC()
			ns := nodes.NewService(dbs.Main, nodes.Options{Now: func() time.Time { return now }})
			node, err := ns.CreatePending(ctx, nodes.CreateRequest{Name: "generation"})
			if err != nil {
				t.Fatal(err)
			}
			en := enrollment.NewService(dbs.Main, enrollment.Options{Now: func() time.Time { return now }})
			ticket, err := en.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := en.Enroll(ctx, ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "generation", NodeToken: coalesceToken}); err != nil {
				t.Fatal(err)
			}
			writeGenerationPublicName(t, ctx, dbs.Main, node.ID, "Original")
			samples := make(chan history.Sample, 8)
			s := NewService(dbs.Main, Options{Now: func() time.Time { return now }, PersistInterval: time.Hour, DisablePersistOffset: true, Samples: samples})
			target := "https://example.com/health"
			report := coalesceReport(now, target)
			report.Traffic = []v1.Traffic{trafficEntry("a", now.Truncate(time.Hour), "100", "200")}
			report.ProbeResults = []v1.ProbeResult{probeResult(target, now, true)}
			if response, err := acceptReport(ctx, s, coalesceToken, report); err != nil || len(response.TrafficAcks) != 1 {
				t.Fatalf("initial report=%+v err=%v", response, err)
			}
			now = now.Add(time.Second)
			report.Seq++
			report.SampledAt = now
			report.Traffic[0].RxBytes = "150"
			report.ProbeResults[0].CheckedAt = now
			if _, err := acceptReport(ctx, s, coalesceToken, report); err != nil {
				t.Fatal(err)
			}
			oldAdmission, err := s.AdmitToken(ctx, coalesceToken)
			if err != nil {
				t.Fatal(err)
			}
			state := s.acquireExistingState(node.ID)
			defer s.releaseState(state)
			if driver == "mysql" {
				// 保留真实已提交收据而延迟内存消费，证明 Tx 对象不占用旧租约。
				lease, err := dbs.Main.Acquire(ctx, storage.Writer)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := lease.BeginWrite(lease.Context())
				if err != nil {
					lease.Close()
					t.Fatal(err)
				}
				traffic, err := state.persistTraffic(lease.Context(), tx, node.ID, now)
				if err != nil {
					tx.Rollback()
					lease.Close()
					t.Fatal(err)
				}
				probes := state.copyProbes(state.credential[:], state.credentialEpoch, report.RunGeneration, report.SessionID)
				if err := probes.persist(lease.Context(), tx, node.ID, now); err != nil {
					tx.Rollback()
					lease.Close()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					lease.Close()
					t.Fatal(err)
				}
				state.rememberReportCommit(tx, lease.Generation(), traffic, probes, nil)
				lease.Close()
			}
			page, err := s.PublicPage(ctx, ns, 100, 0, time.Hour, func() time.Time { return now })
			if err != nil || len(page.Nodes) != 1 || page.Nodes[0].CPUPercent == nil {
				t.Fatalf("initial page=%+v err=%v", page, err)
			}
			if err := dbs.CompleteBootstrap(ctx); err != nil {
				t.Fatal(err)
			}
			if err := dbs.StartReplication(ctx); err != nil {
				t.Fatal(err)
			}
			proofBoot, proofBarrier := writeGenerationPublicName(t, ctx, dbs.Main, node.ID, "Original")
			if proofBoot == "" || proofBarrier <= 0 {
				t.Fatal("guarded post-start transaction did not capture a monotonic barrier")
			}
			t.Logf("proof_boot=%s post_start_barrier_ns=%d", proofBoot, proofBarrier)
			source := remote.RawEndpoint(t, "primary", storage.MainDomain)
			var replica *sql.DB
			if driver == "sqlite" {
				replica, err = storage.OpenReader(ctx, filepath.Join(dir, "orbitbeacon.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer replica.Close()
			} else {
				replica = remote.RawEndpoint(t, "standby", storage.MainDomain)
			}
			waitIngestGeneration(t, ctx, "initial_checkpoint", dbs, source, replica, func() bool {
				var se, si, te, ti, sb, tb string
				var ss, ts, sourceBarrier, targetBarrier, changes int64
				var ready bool
				sourceErr := source.QueryRowContext(ctx, "SELECT s.epoch_uuid,s.head_seq,s.head_tx_uuid,t.boot_id,t.barrier_ns,t.change_count FROM replica_state s JOIN replica_transactions t ON t.epoch_uuid=s.epoch_uuid AND t.seq=s.head_seq AND t.tx_uuid=s.head_tx_uuid WHERE s.id=1").Scan(&se, &ss, &si, &sb, &sourceBarrier, &changes)
				targetErr := replica.QueryRowContext(ctx, "SELECT s.epoch_uuid,s.head_seq,s.head_tx_uuid,s.ready,t.boot_id,t.barrier_ns FROM replica_state s JOIN replica_transactions t ON t.epoch_uuid=s.epoch_uuid AND t.seq=s.head_seq AND t.tx_uuid=s.head_tx_uuid WHERE s.id=1").Scan(&te, &ts, &ti, &ready, &tb, &targetBarrier)
				if sourceErr != nil || targetErr != nil || !ready || se != te || ss != ts || si != ti || sb != proofBoot || tb != proofBoot || sourceBarrier <= proofBarrier || targetBarrier != sourceBarrier || changes != 0 {
					return false
				}
				raw, err := dbs.CaptureRestoreMetadata(ctx)
				var metadata struct {
					Domains map[string]struct {
						Ready   bool
						Rebuild *struct{}
					}
				}
				if err != nil || json.Unmarshal(raw, &metadata) != nil {
					return false
				}
				main, ok := metadata.Domains["main"]
				return ok && main.Ready && main.Rebuild == nil
			})
			before := dbs.Main.Generation()
			s.rateMu.Lock()
			tokens, refilled := state.tokens, state.refilled
			s.rateMu.Unlock()
			proxy.Pause()
			waitIngestGeneration(t, ctx, "promotion", dbs, source, replica, func() bool { return dbs.Main.Generation() > before && dbs.Main.Ready() })
			if _, err := s.AcceptAdmitted(ctx, oldAdmission, report); !errors.Is(err, nodes.ErrUnauthenticated) {
				t.Fatalf("old admission crossed real promotion: %v", err)
			}
			if _, err := s.Latest(ctx, node.ID); !errors.Is(err, nodes.ErrNotFound) {
				t.Fatalf("old latest crossed real promotion: %v", err)
			}
			out, err := publicNode(ctx, ns, s, node.ID, now)
			if err != nil || out.CPUPercent != nil {
				t.Fatalf("old public snapshot crossed real promotion: %+v %v", out, err)
			}
			writeGenerationPublicName(t, ctx, dbs.Main, node.ID, "Promoted")
			page, err = s.PublicPage(ctx, ns, 100, 0, time.Hour, func() time.Time { return now })
			if err != nil || len(page.Nodes) != 1 || page.Nodes[0].Name != "Promoted" || page.Nodes[0].CPUPercent != nil {
				t.Fatalf("old cached page crossed promotion: %+v %v", page, err)
			}
			response, err := acceptReport(ctx, s, coalesceToken, report)
			if err != nil || response.AcceptedSeq != 2 || len(response.TrafficAcks) != 1 || response.TrafficAcks[0].RxBytes != "150" {
				t.Fatalf("retransmit=%+v err=%v", response, err)
			}
			if len(samples) != 2 {
				t.Fatalf("same identity retransmission recounted history: %d", len(samples))
			}
			lease, err := dbs.Main.Acquire(ctx, storage.Reader)
			if err != nil {
				t.Fatal(err)
			}
			var checks, rx int
			err = lease.QueryRowContext(lease.Context(), "SELECT COALESCE(SUM(checks),0) FROM probe_hourly WHERE node_id=? AND target=?", node.ID, target).Scan(&checks)
			if err == nil {
				err = lease.QueryRowContext(lease.Context(), "SELECT rx_bytes FROM traffic_hourly WHERE node_id=?", node.ID).Scan(&rx)
			}
			lease.Close()
			if err != nil || checks != 2 || rx != 150 {
				t.Fatalf("pending replay checks=%d rx=%d err=%v", checks, rx, err)
			}
			s.rateMu.Lock()
			defer s.rateMu.Unlock()
			if state.tokens != tokens-1 || !state.refilled.Equal(refilled) {
				t.Fatal("real promotion reset report rate limiter")
			}
		})
	}
}

func waitIngestGeneration(t *testing.T, ctx context.Context, stage string, dbs *storage.Databases, source, replica *sql.DB, ready func() bool) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ctx.Done():
			logIngestGeneration(t, stage, dbs, source, replica)
			t.Fatalf("real ingest failover did not converge: stage=%s", stage)
		case <-ticker.C:
		}
	}
}

func logIngestGeneration(t *testing.T, stage string, dbs *storage.Databases, source, replica *sql.DB) {
	t.Helper()
	t.Logf("stage=%s active_generation=%d active_ready=%t", stage, dbs.Main.Generation(), dbs.Main.Ready())
	metadataContext, cancelMetadata := context.WithTimeout(context.Background(), 2*time.Second)
	raw, metadataErr := dbs.CaptureRestoreMetadata(metadataContext)
	cancelMetadata()
	var metadata struct {
		Domains map[string]struct {
			Active, State, Reason string
			Ready, Manual         bool
			Freeze                *struct {
				Boot  string
				Nanos int64
				Wall  time.Time
			}
			Intent  *struct{ Pointer bool }
			Rebuild *struct{ Phase string }
		}
	}
	if metadataErr != nil || json.Unmarshal(raw, &metadata) != nil {
		t.Log("main_metadata_available=false")
	} else if main, ok := metadata.Domains["main"]; ok {
		intentPhase := "none"
		if main.Intent != nil {
			intentPhase = "pending_candidate"
			if main.Intent.Pointer {
				intentPhase = "pointer_persisted"
			}
		}
		rebuildPhase := "none"
		if main.Rebuild != nil {
			rebuildPhase = main.Rebuild.Phase
		}
		t.Logf("main_active=%s state=%s reason=%s replica_ready=%t manual=%t freeze=%+v intent_phase=%s rebuild_phase=%s", main.Active, main.State, main.Reason, main.Ready, main.Manual, main.Freeze, intentPhase, rebuildPhase)
	}
	for _, endpoint := range []struct {
		role string
		db   *sql.DB
	}{{"primary", source}, {"standby", replica}} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var epoch, tx string
		var sequence int64
		var complete bool
		var boot sql.NullString
		var barrier sql.NullInt64
		err := endpoint.db.QueryRowContext(ctx, "SELECT s.epoch_uuid,s.head_seq,s.head_tx_uuid,s.ready,t.boot_id,t.barrier_ns FROM replica_state s LEFT JOIN replica_transactions t ON t.epoch_uuid=s.epoch_uuid AND t.seq=s.head_seq AND t.tx_uuid=s.head_tx_uuid WHERE s.id=1").Scan(&epoch, &sequence, &tx, &complete, &boot, &barrier)
		cancel()
		if err != nil {
			kind := fmt.Sprintf("type:%T", err)
			if code, ok := storage.MySQLCode(err); ok {
				kind = fmt.Sprintf("mysql:%d", code)
			} else if code, ok := storage.SQLiteCode(err); ok {
				kind = fmt.Sprintf("sqlite:%d", code)
			} else if errors.Is(err, context.DeadlineExceeded) {
				kind = "deadline_exceeded"
			} else if errors.Is(err, context.Canceled) {
				kind = "canceled"
			}
			t.Logf("role=%s checkpoint_error=%s", endpoint.role, kind)
			continue
		}
		t.Logf("role=%s epoch=%s seq=%d tx=%s ready=%t header_present=%t boot=%s barrier_ns=%d", endpoint.role, epoch, sequence, tx, complete, boot.Valid && barrier.Valid, boot.String, barrier.Int64)
	}
}

func writeGenerationPublicName(t *testing.T, ctx context.Context, db *storage.Database, nodeID, name string) (string, int64) {
	t.Helper()
	lease, err := db.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(lease.Context(), "UPDATE nodes SET name=?,public_settings=? WHERE id=?", name, `{"enabled":true}`, nodeID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var boot string
	var barrier int64
	if err := lease.QueryRowContext(lease.Context(), "SELECT boot_id,barrier_ns FROM replica_transactions WHERE tx_uuid=?", tx.ID()).Scan(&boot, &barrier); err != nil {
		t.Fatal(err)
	}
	return boot, barrier
}
