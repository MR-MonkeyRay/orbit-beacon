package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
)

func planDirectory(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "backups")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}
func localPlanPatch(t *testing.T) PlanPatch {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"path": planDirectory(t)})
	return PlanPatch{Name: new("Nightly"), Kind: new("local"), Destination: raw}
}
func planErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestPlansDefaultsAndInvalidCronAtomic(t *testing.T) {
	db, opts := workerFixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	opts.Now = func() time.Time { return now }
	opts.Location = time.FixedZone("UTC+08", 8*3600)
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := s.SavePlan(ctx, "", localPlanPatch(t))
	if err != nil {
		t.Fatal(err)
	}
	if p.Enabled || p.Cron != "0 3 * * *" || len(p.Categories) != 1 || p.Categories[0] != Configuration || p.RetainCount == nil || *p.RetainCount != 7 || p.NextRunAt != nil {
		t.Fatalf("defaults %+v", p)
	}
	if len(p.NextRuns) != 3 || p.NextRuns[0].Local != "2026-01-02T03:00:00+08:00" {
		t.Fatalf("timezone preview %+v", p.NextRuns)
	}
	for _, raw := range []string{"@daily", "CRON_TZ=UTC 0 3 * * *", "0 0 3 * * *", "0 3 ? * *", "0 3 * * * sh"} {
		_, err = s.SavePlan(ctx, p.ID, PlanPatch{Cron: &raw})
		var fields *validation.Error
		if !errors.As(err, &fields) || fields.Fields["cron"] == "" {
			t.Fatalf("invalid cron %q: %v", raw, err)
		}
	}
	stored, err := s.Plan(ctx, p.ID)
	if err != nil || stored.Cron != p.Cron || !stored.UpdatedAt.Equal(p.UpdatedAt) {
		t.Fatalf("invalid mutation changed plan %+v %v", stored, err)
	}
	enabled := true
	p, err = s.SavePlan(ctx, p.ID, PlanPatch{Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	original := *p.NextRunAt
	now = now.Add(time.Hour)
	p, err = s.SavePlan(ctx, p.ID, PlanPatch{Name: new("Renamed")})
	if err != nil || !p.NextRunAt.Equal(original) {
		t.Fatalf("edit shifted schedule %+v %v", p, err)
	}
	p, err = s.SavePlan(ctx, p.ID, PlanPatch{Cron: new("* * * * *")})
	if err != nil || !p.NextRunAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("cron not reset %+v %v", p, err)
	}
	_, err = s.SavePlan(ctx, p.ID, PlanPatch{Kind: new("object")})
	var fields *validation.Error
	if !errors.As(err, &fields) {
		t.Fatalf("kind mutable %v", err)
	}
}

func TestPlanCronTimezoneDSTAndWeekdaySemantics(t *testing.T) {
	db, opts := workerFixture(t)
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	opts.Location = loc
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ cron, now, want string }{
		{"0 3 * * *", "2026-01-01T00:00:00Z", "2026-01-01T03:00:00-05:00"},
		{"30 2 * * *", "2026-03-08T06:00:00Z", "2026-03-09T02:30:00-04:00"},
		{"30 1 * * *", "2026-11-01T04:00:00Z", "2026-11-01T01:30:00-04:00"},
		{"30 1 * * *", "2026-11-01T05:30:00Z", "2026-11-01T01:30:00-05:00"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		got, err := s.PreviewCron(tc.cron, now)
		if err != nil || got.NextRuns[0].Local != tc.want {
			t.Fatalf("%+v got %+v err %v", tc, got, err)
		}
	}
	opts.Location = time.UTC
	s, err = New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ cron, want string }{
		{"0 0 * * 0", "2026-01-04T00:00:00Z"}, {"0 0 * * 7", "2026-01-04T00:00:00Z"}, {"0 0 * * 0-7", "2026-01-02T00:00:00Z"},
		{"0 0 * * 5-7", "2026-01-02T00:00:00Z"}, {"0 0 * * 1-7/2", "2026-01-02T00:00:00Z"}, {"0 0 2 * MON", "2026-01-02T00:00:00Z"},
		{"0 0 2 * */2", "2026-04-02T00:00:00Z"},
	} {
		got, err := s.PreviewCron(tc.cron, now)
		if err != nil || got.NextRuns[0].At.Format(time.RFC3339) != tc.want {
			t.Fatalf("%s %+v %v", tc.cron, got, err)
		}
	}
}

func TestPlanLocalPathValidation(t *testing.T) {
	db, opts := workerFixture(t)
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	patch := localPlanPatch(t)
	var d struct {
		Path string `json:"path"`
	}
	json.Unmarshal(patch.Destination, &d)
	if err = os.Chmod(d.Path, 0750); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SavePlan(ctx, "", patch); err == nil {
		t.Fatal("permissive directory accepted")
	}
	os.Chmod(d.Path, 0700)
	link := filepath.Join(filepath.Dir(d.Path), "link")
	if err = os.Symlink(d.Path, link); err != nil {
		t.Fatal(err)
	}
	patch.Destination, _ = json.Marshal(map[string]string{"path": link})
	if _, err = s.SavePlan(ctx, "", patch); err == nil {
		t.Fatal("symlink accepted")
	}
	patch.Destination = json.RawMessage(`{"path":"relative"}`)
	if _, err = s.SavePlan(ctx, "", patch); err == nil {
		t.Fatal("relative accepted")
	}
}

func TestPlanObjectSecretsReplacementAndSnapshot(t *testing.T) {
	db, opts := workerFixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	opts.Now = func() time.Time { return now }
	opts.Location = time.UTC
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	patch := PlanPatch{Name: new("Object"), Kind: new("object"), Enabled: new(true), Cron: new("* * * * *"), Destination: json.RawMessage(`{"provider":"s3_compatible","endpoint":"https://objects.example.test","region":"test","bucket":"backups","prefix":"one"}`), Credentials: &ObjectCredentials{AccessKeyID: "key-secret-id", SecretAccessKey: "secret-value", SessionToken: "token-value"}}
	p, err := s.SavePlan(ctx, "", patch)
	if err != nil {
		t.Fatal(err)
	}
	if !p.CredentialsSet || !p.SessionTokenSet {
		t.Fatalf("missing flags %+v", p)
	}
	var dest ObjectDestination
	json.Unmarshal(p.Destination, &dest)
	if !dest.PathStyle {
		t.Fatal("compatible path style default missing")
	}
	p, err = s.SavePlan(ctx, p.ID, PlanPatch{Name: new("Changed")})
	if err != nil || !p.SessionTokenSet {
		t.Fatalf("omitted credentials lost: %v", err)
	}
	raw, _ := json.Marshal(p)
	for _, secret := range []string{"key-secret-id", "secret-value", "token-value", "ciphertext"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret exposed %s", secret)
		}
	}
	_, err = s.SavePlan(ctx, p.ID, PlanPatch{Destination: json.RawMessage(`{"provider":"r2"}`)})
	if err == nil {
		t.Fatal("provider changed without explicit endpoint region credentials")
	}
	_, err = s.SavePlan(ctx, p.ID, PlanPatch{Credentials: &ObjectCredentials{AccessKeyID: "only-id"}})
	if err == nil {
		t.Fatal("partial credentials accepted")
	}
	p, err = s.SavePlan(ctx, p.ID, PlanPatch{Credentials: &ObjectCredentials{AccessKeyID: "new-key", SecretAccessKey: "new-secret"}})
	if err != nil || p.SessionTokenSet {
		t.Fatalf("token not cleared %v", err)
	}
	now = now.Add(11 * time.Minute)
	if err = s.Tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	task, err := scanTask(db.QueryRow(taskSelect+` WHERE plan_id=?`, p.ID))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.box.Open("task-input:"+task.ID, task.inputCredentialsCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(plain)
	var creds ObjectCredentials
	if err = json.Unmarshal(plain, &creds); err != nil || creds.AccessKeyID != "new-key" {
		t.Fatalf("snapshot credentials %+v %v", creds, err)
	}
	for _, action := range []func() error{func() error { _, e := s.SavePlan(ctx, p.ID, PlanPatch{Enabled: new(false)}); return e }, func() error { return s.DeletePlan(ctx, p.ID) }} {
		planErrorCode(t, action(), "backup_plan_busy")
	}
	if _, err = db.Exec(`UPDATE backup_tasks SET state='succeeded',finished_at=? WHERE id=?`, timekey.Format(now), task.ID); err != nil {
		t.Fatal(err)
	}
	summary, err := s.Plan(ctx, p.ID)
	if err != nil || summary.LastTask == nil || summary.LastTask.ID != task.ID || summary.LastTask.State != "succeeded" {
		t.Fatalf("last task summary %+v %v", summary.LastTask, err)
	}
	if _, err = s.SavePlan(ctx, p.ID, PlanPatch{Name: new("Later"), Credentials: &ObjectCredentials{AccessKeyID: "later-id", SecretAccessKey: "later-secret"}, Destination: json.RawMessage(`{"prefix":"two"}`)}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.loadTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshotDestination ObjectDestination
	if err = json.Unmarshal(snapshot.Destination.Target, &snapshotDestination); err != nil {
		t.Fatal(err)
	}
	if snapshot.Destination.PlanName != "Changed" || snapshotDestination.Prefix != "one" {
		t.Fatalf("snapshot changed %+v %v", snapshot, err)
	}
	if err = s.DeletePlan(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.loadTask(ctx, task.ID); err != nil {
		t.Fatal("delete removed history", err)
	}
}

func TestSchedulerCoalescesAndPreservesBlockedDue(t *testing.T) {
	db, opts := workerFixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	opts.Now = func() time.Time { return now }
	opts.Location = time.UTC
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	patch := localPlanPatch(t)
	patch.Enabled = new(true)
	patch.Cron = new("* * * * *")
	p, err := s.SavePlan(ctx, "", patch)
	if err != nil {
		t.Fatal(err)
	}
	due := *p.NextRunAt
	now = now.Add(11 * time.Minute)
	if err = s.Tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	task, err := scanTask(db.QueryRow(taskSelect+` WHERE plan_id=?`, p.ID))
	if err != nil || !task.ScheduledAt.Equal(due) {
		t.Fatalf("missed schedule %+v %v", task, err)
	}
	now = now.Add(11 * time.Minute)
	if err = s.Tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.Plan(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !blocked.NextRunAt.Equal(time.Date(2026, 1, 1, 0, 12, 0, 0, time.UTC)) {
		t.Fatal("active advanced next run")
	}
	if _, err = db.Exec(`UPDATE backup_tasks SET state='failed',finished_at=? WHERE id=?`, timekey.Format(now), task.ID); err != nil {
		t.Fatal(err)
	}
	for range 32 {
		if _, err = s.CreateExport(ctx, restoreUser, []Category{Configuration}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	full, err := s.Plan(ctx, p.ID)
	if err != nil || !full.NextRunAt.Equal(*blocked.NextRunAt) {
		t.Fatalf("full advanced due %+v %v", full, err)
	}
	var scheduled int
	if err = db.QueryRow(`SELECT count(*) FROM backup_tasks WHERE plan_id=?`, p.ID).Scan(&scheduled); err != nil || scheduled != 1 {
		t.Fatalf("full queue added task count=%d err=%v", scheduled, err)
	}
	if _, err = db.Exec(`UPDATE backup_tasks SET state='failed',finished_at=? WHERE id=(SELECT id FROM backup_tasks WHERE state='queued' LIMIT 1)`, timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM backup_tasks WHERE plan_id=?`, p.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("coalescing count=%d %v", count, err)
	}
}

func TestPlansPaginationAndLastTask(t *testing.T) {
	db, opts := workerFixture(t)
	opts.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 3 {
		if _, err = s.SavePlan(ctx, "", localPlanPatch(t)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.Plans(ctx, 2, "")
	if err != nil || !page.HasMore || page.NextCursor == nil || len(page.Plans) != 2 {
		t.Fatalf("page %+v %v", page, err)
	}
	next, err := s.Plans(ctx, 2, *page.NextCursor)
	if err != nil || next.HasMore || len(next.Plans) != 1 {
		t.Fatalf("next %+v %v", next, err)
	}
	for _, p := range page.Plans {
		if p.ID == next.Plans[0].ID {
			t.Fatal("duplicate page")
		}
	}
	if _, err = s.Plans(ctx, 101, ""); err == nil {
		t.Fatal("unbounded list")
	}
	if _, err = s.Plans(ctx, 10, "bad"); err == nil {
		t.Fatal("invalid cursor")
	}
}
