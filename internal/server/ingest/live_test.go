package ingest

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"orbitbeacon/internal/timekey"
)

func TestLiveFleetProjectsIdentityAwareMemoryWithoutDecodingReports(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	f.now = f.now.Add(10 * time.Second)
	f.report.Seq++
	if _, err := acceptReport(context.Background(), f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	records, err := f.nodes.LiveRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state := f.service.acquireExistingState(f.id)
	state.mu.Lock()
	state.latest = []byte("intentionally not JSON: fleet must not decode full reports")
	state.mu.Unlock()
	f.service.releaseState(state)
	fleet := f.service.ProjectLiveRecord(&records[0], f.now)
	if fleet.Presence != "online" || fleet.LastSeen == nil || !fleet.LastSeen.Equal(f.now) {
		t.Fatalf("coalesced fleet=%+v", fleet)
	}
	for _, mutation := range []string{"credential", "epoch", "generation", "session"} {
		t.Run(mutation, func(t *testing.T) {
			record := records[0]
			switch mutation {
			case "credential":
				record.Identity.Credential = sha256.Sum256([]byte("other"))
			case "epoch":
				record.Identity.CredentialEpoch++
			case "generation":
				record.Identity.Generation++
			case "session":
				record.Identity.Session = "replacement"
			}
			fleet := f.service.ProjectLiveRecord(&record, f.now)
			if fleet.LastSeen == nil || !fleet.LastSeen.Equal(*record.LastSeen) {
				t.Fatalf("%s overlaid old memory: %+v", mutation, fleet)
			}
		})
	}
}

func TestLiveDetailResetAndABARejectOldMemoryAndPreserveRestartRuntime(t *testing.T) {
	for _, mutation := range []string{"revoke", "aba", "new-run", "restart"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCoalesceFixture(t, 30*time.Second)
			ctx := context.Background()
			switch mutation {
			case "revoke":
				if err := f.nodes.RevokeCredential(ctx, f.id); err != nil {
					t.Fatal(err)
				}
			case "aba":
				if _, err := f.db.Exec("UPDATE nodes SET credential_epoch=credential_epoch+2,last_seen=NULL,system='',agent_runtime='',run_generation='0',report_session_id='' WHERE id=?", f.id); err != nil {
					t.Fatal(err)
				}
			case "new-run":
				if _, err := f.db.Exec("UPDATE nodes SET run_generation='2',report_session_id='next',agent_runtime='' WHERE id=?", f.id); err != nil {
					t.Fatal(err)
				}
			case "restart":
				f.service = NewService(f.db, Options{Now: func() time.Time { return f.now }})
			}
			record, err := f.nodes.LiveDetail(ctx, f.id)
			if err != nil {
				t.Fatal(err)
			}
			latest, err := f.service.ProjectLiveDetail(&record, f.now)
			if err != nil {
				t.Fatal(err)
			}
			if latest != nil {
				t.Fatalf("%s exposed old latest: %+v", mutation, latest)
			}
			if mutation == "restart" {
				if record.Runtime == nil || record.Node.Presence != "online" {
					t.Fatalf("restart lost durable observations: %+v", record)
				}
			} else if record.Runtime != nil {
				t.Fatalf("%s exposed old runtime: %+v", mutation, record.Runtime)
			}
			if mutation == "revoke" || mutation == "aba" {
				if record.Node.LastSeen != nil || (record.Node.Presence != "pending" && record.Node.Presence != "never") {
					t.Fatalf("identity reset repopulated observations: %+v", record.Node)
				}
			}
		})
	}
}

func TestLiveDetailUsesOneAcceptedSamplesRuntimeAcrossDurableCommit(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	record, err := f.nodes.LiveDetail(context.Background(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.Runtime.ReportIntervalSeconds = 5
	if _, err := acceptReport(context.Background(), f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	latest, err := f.service.ProjectLiveDetail(&record, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if latest != nil && latest.Runtime.ReportIntervalSeconds != record.Runtime.ReportIntervalSeconds {
		t.Fatalf("mixed runtime/latest: latest=%+v runtime=%+v", latest.Runtime, record.Runtime)
	}
	record, err = f.nodes.LiveDetail(context.Background(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	latest, err = f.service.ProjectLiveDetail(&record, f.now)
	if err != nil || latest == nil || record.Runtime.ReportIntervalSeconds != 5 || latest.Runtime.ReportIntervalSeconds != 5 {
		t.Fatalf("current detail runtime/latest=%+v/%+v err=%v", record.Runtime, latest, err)
	}
}

func TestLiveFleetRejectsLateFastPathMemoryAfterCredentialReset(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	ctx := context.Background()
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	entered, resume := make(chan struct{}), make(chan struct{})
	f.service.now = func() time.Time { close(entered); <-resume; return f.now }
	result := make(chan error, 1)
	credential := sha256.Sum256([]byte(coalesceToken))
	go func() { _, err := f.service.accept(ctx, f.id, credential[:], 0, f.report); result <- err }()
	<-entered
	if err := f.nodes.RevokeCredential(ctx, f.id); err != nil {
		close(resume)
		t.Fatal(err)
	}
	close(resume)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	records, err := f.nodes.LiveRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fleet := f.service.ProjectLiveRecord(&records[0], f.now)
	if fleet.Presence != "pending" || fleet.LastSeen != nil {
		t.Fatalf("late admitted memory repopulated reset fleet: %+v", fleet)
	}
	record, err := f.nodes.LiveDetail(ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := f.service.ProjectLiveDetail(&record, f.now)
	if err != nil || latest != nil || record.Runtime != nil || record.Node.LastSeen != nil {
		t.Fatalf("late admitted memory repopulated detail: %+v latest=%+v err=%v", record, latest, err)
	}
}

func TestLiveDetailPrefersAcceptedSampleRuntimeDuringDurablePublicationGap(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	// 下一条报告的持久化事务已提交，但其已接受的内存快照尚未发布。
	// 上一份已接受样本在相同完整进程身份下仍然有效。
	f.now = f.now.Add(time.Second)
	if _, err := f.db.Exec(`UPDATE nodes SET last_seen=?,agent_runtime='{"report_interval_seconds":5,"disk_interval_seconds":60,"interfaces":[],"disk_paths":[],"probe_targets":[]}' WHERE id=?`, timekey.Format(f.now), f.id); err != nil {
		t.Fatal(err)
	}
	record, err := f.nodes.LiveDetail(context.Background(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Runtime == nil || record.Runtime.ReportIntervalSeconds != 5 {
		t.Fatalf("fixture runtime=%+v", record.Runtime)
	}
	latest, err := f.service.ProjectLiveDetail(&record, f.now)
	if err != nil || latest == nil {
		t.Fatalf("accepted current-identity sample disappeared: latest=%+v err=%v", latest, err)
	}
	if record.Runtime.ReportIntervalSeconds != 10 || latest.Runtime.ReportIntervalSeconds != 10 {
		t.Fatalf("old sample paired with newly durable runtime: runtime=%+v latest=%+v", record.Runtime, latest.Runtime)
	}
}
