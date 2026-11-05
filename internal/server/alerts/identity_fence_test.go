package alerts

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

type identityFenceReader struct {
	LatestReader
	calls  int
	report ingest.LatestReport
	err    error
}

func (r *identityFenceReader) Latest(ctx context.Context, id string) (ingest.LatestReport, error) {
	r.calls++
	r.report, r.err = r.LatestReader.Latest(ctx, id)
	return r.report, r.err
}

func TestEvaluatorRealLatestFencesIdentityMutations(t *testing.T) {
	for _, mutation := range []string{"revoke", "disable", "credential_epoch", "run_generation", "report_session_id", "reenroll-same-token", "delete-recreate-same-id"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, 0)
			token := strings.Repeat("A", 43)
			credential := sha256.Sum256([]byte(token))
			f.exec(t, "UPDATE nodes SET token_hash=? WHERE id=?", credential[:], f.node)
			real := ingest.NewService(f.db, ingest.Options{Now: func() time.Time { return f.now }})
			report := f.latest.report.Report
			report.ProtocolVersion, report.RunGeneration, report.Seq = 1, 1, 1
			report.SessionID, report.SampledAt = "one", f.now
			// 沿用 fixture 的 CPU 越限值，其余指标明确标为不可用。
			for _, field := range []string{"load1", "load5", "load15", "uptime_seconds", "memory_total_bytes", "memory_available_bytes", "swap_total_bytes", "swap_used_bytes"} {
				report.Metrics.MetricStatus[field] = "error"
			}
			for _, family := range []string{"load", "memory", "swap", "uptime"} {
				report.Metrics.Sources[family] = v1.SourceMetadata{SampleID: family, SourceIntervalMS: 10000}
			}
			cpu := report.Metrics.Sources["cpu"]
			cpu.MeasurementWindowMS = ptr(int64(10000))
			report.Metrics.Sources["cpu"] = cpu
			accept := func(s *ingest.Service, token string) {
				t.Helper()
				admission, err := s.AdmitToken(ctx, token)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.AcceptAdmitted(ctx, admission, report); err != nil {
					t.Fatal(err)
				}
			}
			accept(real, token)
			// 相同输入必须确实能触发，排除指标格式或时效错误导致的假绿。
			f.e = NewEvaluator(f.s, real, EvaluatorOptions{})
			f.tick(t)
			if events := f.events(t, "firing"); len(events) != 1 || events[0].Value == nil || *events[0].Value != 90 {
				t.Fatalf("matching identity did not fire: %+v", events)
			}
			f.exec(t, "DELETE FROM alert_events")
			reader := &identityFenceReader{LatestReader: real}
			f.e = NewEvaluator(f.s, reader, EvaluatorOptions{})
			ns := nodes.NewService(f.db, nodes.Options{Now: func() time.Time { return f.now }})
			switch mutation {
			case "revoke":
				if err := ns.RevokeCredential(ctx, f.node); err != nil {
					t.Fatal(err)
				}
			case "disable":
				if err := ns.SetDisabled(ctx, f.node, true); err != nil {
					t.Fatal(err)
				}
			case "credential_epoch":
				f.exec(t, "UPDATE nodes SET credential_epoch=credential_epoch+1 WHERE id=?", f.node)
			case "run_generation":
				f.exec(t, "UPDATE nodes SET run_generation='2' WHERE id=?", f.node)
			case "report_session_id":
				f.exec(t, "UPDATE nodes SET report_session_id='replacement' WHERE id=?", f.node)
			case "reenroll-same-token":
				if err := ns.RevokeCredential(ctx, f.node); err != nil {
					t.Fatal(err)
				}
				es := enrollment.NewService(f.db, enrollment.Options{Now: func() time.Time { return f.now }})
				ticket, err := es.CreateSingle(ctx, f.node)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := es.Enroll(ctx, ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "replacement", NodeToken: token}); err != nil {
					t.Fatal(err)
				}
				// 新实例推进持久化身份，原实例的 CPU=90 仍留在内存。
				report.Metrics.CPUPercent = ptr(10.0)
				accept(ingest.NewService(f.db, ingest.Options{Now: func() time.Time { return f.now }}), token)
			case "delete-recreate-same-id":
				if err := ns.Delete(ctx, f.node, true); err != nil {
					t.Fatal(err)
				}
				credential = sha256.Sum256([]byte(strings.Repeat("B", 43)))
				f.exec(t, `INSERT INTO nodes(id,name,status,token_hash,first_enrolled_at,last_seen,created_at,run_generation,report_session_id) VALUES(?,'replacement','unknown',?,?,?,?,'1','one')`, f.node, credential[:], timekey.Format(f.now), timekey.Format(f.now), timekey.Format(f.now))
			}
			f.tick(t)
			// 停用和撤销也会被规则过滤；同时验证实际读取结果，避免过滤掩盖 Latest 围栏失效。
			if reader.calls != 1 || !errors.Is(reader.err, nodes.ErrNotFound) || reader.report.Metrics.CPUPercent != nil {
				t.Fatalf("evaluator read old identity: calls=%d report=%+v err=%v", reader.calls, reader.report, reader.err)
			}
			if events := f.events(t, "all"); len(events) != 0 || len(f.e.pending) != 0 || len(f.e.queue) != 0 {
				t.Fatalf("old report reached evaluator: events=%+v pending=%d queued=%d", events, len(f.e.pending), len(f.e.queue))
			}
		})
	}
}
