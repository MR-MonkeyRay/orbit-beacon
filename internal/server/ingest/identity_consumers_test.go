package ingest

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func TestLatestAndPublicProjectionFenceReplacementIdentity(t *testing.T) {
	for _, replacement := range []string{"reenroll-same-token", "delete-recreate-same-id"} {
		t.Run(replacement, func(t *testing.T) {
			ctx := context.Background()
			f := newCoalesceFixture(t, 0)
			before, err := publicNode(ctx, f.nodes, f.service, f.id, f.now)
			if err != nil || before.CPUPercent == nil || *before.CPUPercent != 10 {
				t.Fatalf("initial public report=%+v err=%v", before, err)
			}
			token := coalesceToken
			if replacement == "reenroll-same-token" {
				if err := f.nodes.RevokeCredential(ctx, f.id); err != nil {
					t.Fatal(err)
				}
				es := enrollment.NewService(f.db, enrollment.Options{Now: func() time.Time { return f.now }})
				ticket, err := es.CreateSingle(ctx, f.id)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := es.Enroll(ctx, ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "replacement", NodeToken: token}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.nodes.Delete(ctx, f.id, true); err != nil {
					t.Fatal(err)
				}
				token = strings.Repeat("B", 43)
				credential := sha256.Sum256([]byte(token))
				// 有意复用主键，避免仅靠新节点 ID 隔离而掩盖内存身份比对失效。
				if _, err := f.db.Exec(`INSERT INTO nodes(id,name,status,token_hash,first_enrolled_at,created_at,public_settings) VALUES(?,'replacement','unknown',?,?,?,'{"enabled":true,"name":"Replacement"}')`, f.id, credential[:], timekey.Format(f.now), timekey.Format(f.now)); err != nil {
					t.Fatal(err)
				}
			}
			// 另一 ingest 实例已接受新身份的报告；代次、会话和接收时间与旧身份相同。
			// 原实例仍持有旧报告，因此只有凭据或 epoch 围栏能阻止串用。
			current := NewService(f.db, Options{Now: func() time.Time { return f.now }})
			report := f.report
			report.Metrics.CPUPercent = new(20.0)
			if _, err := acceptReport(ctx, current, token, report); err != nil {
				t.Fatal(err)
			}
			if latest, err := current.Latest(ctx, f.id); err != nil || latest.Metrics.CPUPercent == nil || *latest.Metrics.CPUPercent != 20 {
				t.Fatalf("replacement report=%+v err=%v", latest, err)
			}
			if latest, err := f.service.Latest(ctx, f.id); !errors.Is(err, nodes.ErrNotFound) {
				t.Errorf("replacement inherited old latest=%+v err=%v", latest, err)
			}
			out, err := publicNode(ctx, f.nodes, f.service, f.id, f.now)
			if err != nil || out.Status != "online" || out.CPUPercent != nil || out.MemoryPercent != nil || out.Load1 != nil || out.UptimeSeconds != nil {
				t.Fatalf("replacement inherited old public metrics=%+v err=%v", out, err)
			}
			assertLiveMetrics(t, liveFleetNode(t, f), nil, nil)
			assertPublicStatuses(t, out, "unavailable", "unavailable", "unavailable", "unavailable")
		})
	}
}
