package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/timekey"
)

type backupHTTPFixture struct {
	fixture
	service *backup.Service
	box     *secretbox.Box
	dir     string
	now     time.Time
	logs    *bytes.Buffer
}

func newBackupHTTP(t *testing.T) backupHTTPFixture {
	t.Helper()
	f := newHTTP(t, 5)
	var seq int
	var name, path string
	if err := f.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := datadir.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	key, err := datadir.LoadKey(filepath.Join(dir, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key, "orbitbeacon backup secrets v1")
	clear(key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	s, err := backup.New(f.db, backup.Options{DataDir: dir, SecretBox: box, Now: func() time.Time { return now }, Location: time.FixedZone("Server/+02", 7200), ServerVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	logs := new(bytes.Buffer)
	h, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Backups: s, PublicURL: publicURL, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(logs, nil)), AccessLog: true})
	if err != nil {
		t.Fatal(err)
	}
	f.handler = h
	return backupHTTPFixture{fixture: f, service: s, box: box, dir: dir, now: now, logs: logs}
}

const backupHTTPMissingID = "00000000-0000-4000-8000-000000000009"

func TestBackupsHTTPAuthenticationProtectionAndRoutePrecedence(t *testing.T) {
	f := newBackupHTTP(t)
	second, csrf := login(t, f.fixture, "admin-two")
	for _, route := range []struct{ method, path string }{
		{"POST", "/backups/exports"}, {"GET", "/backup-plans"}, {"POST", "/backup-plans"}, {"POST", "/backup-plans/preview"},
		{"GET", "/backup-plans/" + backupHTTPMissingID}, {"PATCH", "/backup-plans/" + backupHTTPMissingID}, {"DELETE", "/backup-plans/" + backupHTTPMissingID},
		{"GET", "/tasks"}, {"GET", "/tasks/" + backupHTTPMissingID}, {"GET", "/tasks/" + backupHTTPMissingID + "/download"}, {"POST", "/tasks/" + backupHTTPMissingID + "/password"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			apiError(t, request(t, f.handler, route.method, "/api/v1"+route.path, `{}`, nil, "", publicURL), 401, "unauthenticated")
			if route.method != "GET" {
				apiError(t, request(t, f.handler, route.method, "/api/v1"+route.path, `{}`, second, "", publicURL), 403, "forbidden")
				apiError(t, request(t, f.handler, route.method, "/api/v1"+route.path, `{}`, second, csrf, "https://foreign.test"), 403, "forbidden")
			}
		})
	}
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/backups/exports", "", nil, "", ""), 405)
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/tasks/"+backupHTTPMissingID+"/password", "", nil, "", ""), 405)
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/tasks/not-a-uuid/download", "", nil, "", ""), 404)
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/tasks/"+backupHTTPMissingID+"/unknown", `{}`, nil, "", ""), 404)
}

func TestBackupsHTTPExportAcceptanceAndStrictInput(t *testing.T) {
	f := newBackupHTTP(t)
	admin, csrf := login(t, f.fixture, "admin-two")
	for _, body := range []string{`{}`, `{"categories":null}`, `{"categories":[]}`, `{"categories":["configuration","configuration"]}`, `{"categories":["unknown"]}`} {
		r := request(t, f.handler, "POST", "/api/v1/backups/exports", body, admin, csrf, publicURL)
		object := apiError(t, r, 400, "validation_failed")
		if !bytes.Contains(object["fields"], []byte("categories")) {
			t.Error("missing categories field error")
		}
	}
	apiError(t, request(t, f.handler, "POST", "/api/v1/backups/exports", `{"categories":["events"],"password":"never"}`, admin, csrf, publicURL), 400, "invalid_json")
	apiError(t, request(t, f.handler, "POST", "/api/v1/backups/exports", `{"categories":["configuration"]}`, admin, "", publicURL), 403, "forbidden")
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", publicURL+"/api/v1/backups/exports", strings.NewReader(`{"categories":["configuration"]}`)).WithContext(ctx)
	req.AddCookie(admin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", publicURL)
	req.Header.Set("X-CSRF-Token", csrf)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	assertHTTPStatus(t, response, 202)
	id := objectID(t, decodeObject(t, response))
	if response.Header().Get("Location") != "/api/v1/tasks/"+id {
		t.Fatal("missing task Location")
	}
	cancel()
	workerCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.service.Run(workerCtx) }()
	defer func() { stop(); <-done }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		task, err := f.service.Task(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == "succeeded" {
			break
		}
		if task.State == "failed" || time.Now().After(deadline) {
			t.Fatalf("export did not complete: %+v", task)
		}
		time.Sleep(10 * time.Millisecond)
	}
	collectionContains(t, request(t, f.handler, "GET", "/api/v1/tasks", "", admin, "", ""), "tasks", id)
	first, _ := login(t, f.fixture, "admin")
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/tasks/"+id, "", first, "", ""), 200)
	stop()
	<-done
	for range 32 {
		assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/backups/exports", `{"categories":["events"]}`, admin, csrf, publicURL), 202)
	}
	apiError(t, request(t, f.handler, "POST", "/api/v1/backups/exports", `{"categories":["events"]}`, admin, csrf, publicURL), 429, "backup_queue_full")
}

func TestBackupsHTTPRejectsEveryUnexpectedQuery(t *testing.T) {
	f := newBackupHTTP(t)
	admin, csrf := login(t, f.fixture, "admin")
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/tasks", ""}, {"GET", "/backup-plans", ""}, {"GET", "/tasks/" + backupHTTPMissingID, ""}, {"GET", "/tasks/" + backupHTTPMissingID + "/download", ""},
		{"POST", "/tasks/" + backupHTTPMissingID + "/password", `{}`}, {"GET", "/backup-plans/" + backupHTTPMissingID, ""},
		{"PATCH", "/backup-plans/" + backupHTTPMissingID, `{}`}, {"DELETE", "/backup-plans/" + backupHTTPMissingID, `{}`}, {"POST", "/backup-plans/preview", `{"cron":"0 3 * * *"}`},
		{"POST", "/backup-plans", `{}`}, {"POST", "/backups/exports", `{"categories":["events"]}`},
	} {
		for _, query := range []string{"unknown=x", "limit=", "before=", "limit=1&limit=2", "before=x&before=y"} {
			apiError(t, request(t, f.handler, route.method, "/api/v1"+route.path+"?"+query, route.body, admin, csrf, publicURL), 400, "invalid_input")
		}
	}
	for _, path := range []string{"tasks", "backup-plans"} {
		for _, query := range []string{"limit=0", "limit=101", "limit=no", "before=invalid", "limit=%ZZ"} {
			apiError(t, request(t, f.handler, "GET", "/api/v1/"+path+"?"+query, "", admin, "", ""), 400, "invalid_input")
		}
	}
}

func seedBackupHTTPArtifact(t *testing.T, f backupHTTPFixture, id string) (string, string) {
	t.Helper()
	name := "orbitbeacon-20260304T050607Z-" + id + ".tar.gz.age"
	password := "A1b2C3d4E5f6G7h8"
	cipher, err := f.box.Seal("task-password:"+id, []byte(password))
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	expires := timekey.Format(f.now.Add(24 * time.Hour))
	_, err = f.db.Exec(`INSERT INTO backup_tasks(id,kind,state,phase,categories_json,destination_json,created_at,finished_at,artifact_name,artifact_bytes,artifact_sha256,artifact_expires_at,result_location,password_ciphertext,password_expires_at) VALUES(?,'backup_export','succeeded','storing','["configuration"]','{"actor_name":"admin","target":null}',?,?,?,10,?,?,?,?,?)`, id, timekey.Format(f.now), timekey.Format(f.now), name, hash, expires, "exports/"+id+"/"+name, cipher, timekey.Format(f.now.Add(90*24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.dir, "exports", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	return path, password
}

func TestBackupsHTTPArtifactRangeExpiryPasswordAndAudit(t *testing.T) {
	f := newBackupHTTP(t)
	admin, csrf := login(t, f.fixture, "admin")
	id := backupHTTPMissingID
	path, secret := seedBackupHTTPArtifact(t, f, id)
	base := "/api/v1/tasks/" + id
	response := request(t, f.handler, "GET", base, "", admin, "", "")
	assertHTTPStatus(t, response, 200)
	object := decodeObject(t, response)
	if object["artifact_bytes"] != "10" || object["download_available"] != true || object["password_available"] != true {
		t.Fatalf("metadata = %+v", object)
	}
	req := httptest.NewRequest("GET", publicURL+base+"/download", nil)
	req.AddCookie(admin)
	req.Header.Set("Range", "bytes=2-5")
	response = httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	assertHTTPStatus(t, response, 206)
	if response.Body.String() != "2345" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(response.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("unsafe range response: %v %q", response.Header(), response.Body.String())
	}
	response = request(t, f.handler, "POST", base+"/password", "", admin, csrf, publicURL)
	assertHTTPStatus(t, response, 200)
	var revealed backup.PasswordResult
	if err := json.Unmarshal(response.Body.Bytes(), &revealed); err != nil {
		t.Fatal(err)
	}
	if revealed.Password != secret || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("explicit password response missing or cacheable")
	}
	var audits int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='backup.password' AND target_id=?`, id).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("password audit=%d err=%v", audits, err)
	}
	if _, err := f.db.Exec(`UPDATE backup_tasks SET password_ciphertext=x'010203' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	apiError(t, request(t, f.handler, "POST", base+"/password", "", admin, csrf, publicURL), 409, "secret_unreadable")
	if _, err := f.db.Exec(`UPDATE backup_tasks SET password_expires_at=?,artifact_expires_at=? WHERE id=?`, timekey.Format(f.now), timekey.Format(f.now), id); err != nil {
		t.Fatal(err)
	}
	apiError(t, request(t, f.handler, "POST", base+"/password", "", admin, csrf, publicURL), 410, "password_expired")
	apiError(t, request(t, f.handler, "GET", base+"/download", "", admin, "", ""), 410, "artifact_expired")
	if _, err := f.db.Exec(`UPDATE backup_tasks SET artifact_expires_at=? WHERE id=?`, timekey.Format(f.now.Add(time.Hour)), id); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	apiError(t, request(t, f.handler, "GET", base+"/download", "", admin, "", ""), 410, "artifact_missing")
	if _, err := f.db.Exec(`UPDATE backup_tasks SET state='queued' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	apiError(t, request(t, f.handler, "GET", base+"/download", "", admin, "", ""), 409, "artifact_not_ready")
	apiError(t, request(t, f.handler, "POST", base+"/password", "", admin, csrf, publicURL), 404, "password_unavailable")
	var details string
	if err := f.db.QueryRow(`SELECT coalesce(group_concat(details),'') FROM audit_events`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{details, f.logs.String()} {
		if strings.Contains(output, secret) || strings.Contains(output, "010203") || strings.Contains(output, f.dir) {
			t.Error("secret or internal path leaked into logs")
		}
	}
}

func TestBackupsHTTPPlanCRUDPreviewPaginationAndBusy(t *testing.T) {
	f := newBackupHTTP(t)
	admin, csrf := login(t, f.fixture, "admin-two")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"name":"Nightly","kind":"local","destination":{"path":%q},"categories":["events"],"cron":"0 3 * * *"}`, dir)
	response := request(t, f.handler, "POST", "/api/v1/backup-plans", body, admin, csrf, publicURL)
	assertHTTPStatus(t, response, 201)
	id := objectID(t, decodeObject(t, response))
	base := "/api/v1/backup-plans/" + id
	assertHTTPStatus(t, request(t, f.handler, "GET", base, "", admin, "", ""), 200)
	first, _ := login(t, f.fixture, "admin")
	assertHTTPStatus(t, request(t, f.handler, "GET", base, "", first, "", ""), 200)
	response = request(t, f.handler, "POST", "/api/v1/backup-plans/preview", `{"cron":"0 3 * * *"}`, admin, csrf, publicURL)
	assertHTTPStatus(t, response, 200)
	preview := decodeObject(t, response)
	if preview["timezone_name"] != "Server/+02" || preview["server_time"] != f.now.Format(time.RFC3339) || len(preview["next_runs"].([]any)) != 3 {
		t.Fatalf("preview=%+v", preview)
	}
	apiError(t, request(t, f.handler, "POST", "/api/v1/backup-plans/preview", `{"cron":"@daily"}`, admin, csrf, publicURL), 400, "validation_failed")
	for _, patch := range []string{`{"categories":null}`, `{"credentials":null}`, `{"destination":null}`, `{"name":null}`, `{"enabled":null}`} {
		assertHTTPStatus(t, request(t, f.handler, "PATCH", base, patch, admin, csrf, publicURL), 400)
	}
	assertHTTPStatus(t, request(t, f.handler, "PATCH", base, `{"unknown":true}`, admin, csrf, publicURL), 400)
	assertHTTPStatus(t, request(t, f.handler, "PATCH", base, `{"name":"Renamed","enabled":true}`, admin, csrf, publicURL), 200)
	if err := f.service.Tick(context.Background(), f.now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	apiError(t, request(t, f.handler, "PATCH", base, `{"name":"Busy"}`, admin, csrf, publicURL), 409, "backup_plan_busy")
	apiError(t, request(t, f.handler, "DELETE", base, "", admin, csrf, publicURL), 409, "backup_plan_busy")
	if _, err := f.db.Exec(`UPDATE backup_tasks SET state='failed',finished_at=? WHERE plan_id=?`, timekey.Format(f.now), id); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, request(t, f.handler, "DELETE", base, "", admin, csrf, publicURL), 204)
	apiError(t, request(t, f.handler, "GET", base, "", admin, "", ""), 404, "backup_plan_not_found")
	remote := `{"name":"Cloud","kind":"object","destination":{"provider":"s3_compatible","endpoint":"https://objects.example.test","region":"test","bucket":"backups","prefix":"safe","path_style":true},"credentials":{"access_key_id":"HTTP-ACCESS-SECRET","secret_access_key":"HTTP-KEY-SECRET","session_token":"HTTP-TOKEN-SECRET"}}`
	for range 2 {
		assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/backup-plans", remote, admin, csrf, publicURL), 201)
	}
	response = request(t, f.handler, "GET", "/api/v1/backup-plans?limit=1", "", admin, "", "")
	assertHTTPStatus(t, response, 200)
	page := decodeObject(t, response)
	if page["has_more"] != true || page["next_cursor"] == nil || len(page["plans"].([]any)) != 1 {
		t.Fatalf("page=%+v", page)
	}
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/backup-plans?limit=1&before="+page["next_cursor"].(string), "", admin, "", ""), 200)
	for _, secret := range []string{"HTTP-ACCESS-SECRET", "HTTP-KEY-SECRET", "HTTP-TOKEN-SECRET"} {
		if strings.Contains(response.Body.String(), secret) || strings.Contains(f.logs.String(), secret) {
			t.Error("plan credentials exposed")
		}
	}
}
