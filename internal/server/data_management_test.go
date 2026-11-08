package server_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"encoding/json"
	"net/http"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
	"testing"
)

func TestDataManagementStatisticsSelectiveCleanupPreservesLiveData(t *testing.T) {
	f := newHTTP(t, 3)
	h, err := server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, Now: func() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }}))
	if err != nil {
		t.Fatal(err)
	}
	f.handler = h
	cookie, csrf := login(t, f, "admin")
	for _, row := range []struct{ id, created string }{{"old-audit", "2000-01-01T00:00:00.000000000Z"}, {"live-audit", "2999-01-01T00:00:00.000000000Z"}} {
		if _, err := f.db.Exec("INSERT INTO audit_events(id,action,target_id,created_at,details) VALUES (?,'fixture','',?,'{}')", row.id, row.created); err != nil {
			t.Fatal(err)
		}
	}
	var sessions, users int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&users); err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, http.MethodGet, "/api/v1/data-management?keep_days=30", "", cookie, "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("stats: %d %s", response.Code, response.Body)
	}
	var stats struct {
		Before    string `json:"before"`
		Databases []struct {
			Domain, Driver string
			Size           int64 `json:"size_bytes"`
		} `json:"databases"`
		Categories []struct {
			ID       string `json:"id"`
			Rows     int64  `json:"rows"`
			Eligible int64  `json:"eligible_rows"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Before == "" || len(stats.Databases) != 1 || stats.Databases[0].Domain != "main" || stats.Databases[0].Driver != "sqlite" || stats.Databases[0].Size <= 0 {
		t.Fatalf("invalid stats: %+v", stats)
	}
	found := false
	for _, category := range stats.Categories {
		if category.ID == "audit_events" {
			found = true
			if category.Eligible != 1 || category.Rows < 2 {
				t.Fatalf("audit counts: %+v", category)
			}
		}
		if category.ID == "metric_history" {
			t.Fatal("fabricated missing history")
		}
	}
	if !found {
		t.Fatal("missing audit category")
	}
	response = request(t, f.handler, http.MethodPost, "/api/v1/data-management/cleanup", `{"categories":["audit_events"],"keep_days":30,"confirmation":"CLEAN","before":"`+stats.Before+`"}`, cookie, csrf, publicURL)
	if response.Code != http.StatusOK {
		t.Fatalf("cleanup: %d %s", response.Code, response.Body)
	}
	var result struct {
		Results []struct {
			ID      string `json:"id"`
			Deleted int64  `json:"deleted_rows"`
			More    bool   `json:"has_more"`
		} `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].ID != "audit_events" || result.Results[0].Deleted != 1 || result.Results[0].More {
		t.Fatalf("cleanup result: %+v", result)
	}
	for _, check := range []struct {
		query string
		want  int
	}{{"SELECT COUNT(*) FROM audit_events WHERE id='old-audit'", 0}, {"SELECT COUNT(*) FROM audit_events WHERE id='live-audit'", 1}, {"SELECT COUNT(*) FROM sessions", sessions}, {"SELECT COUNT(*) FROM users", users}} {
		var got int
		if err := f.db.QueryRow(check.query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != check.want {
			t.Fatalf("%s = %d, want %d", check.query, got, check.want)
		}
	}
}

func TestDataManagementAuthAndValidationNeverDelete(t *testing.T) {
	f := newHTTP(t, 3)
	_, token, cookie, csrf := createHTTPAPIToken(t, f)
	if _, err := f.db.Exec("INSERT INTO audit_events(id,action,target_id,created_at,details) VALUES('protected-old','fixture','','2000-01-01T00:00:00.000000000Z','{}')"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/data-management", "/api/v1/data-management/cleanup", "/api/v1/data-management/compact"} {
		verb := "POST"
		if path == "/api/v1/data-management" {
			verb = "GET"
		}
		apiError(t, request(t, f.handler, verb, path, `{}`, nil, "", publicURL), 401, "unauthenticated")
		apiError(t, apiTokenRequest(t, f.handler, verb, path, "Bearer "+token, cookie), 403, "token_forbidden")
		if verb == "POST" {
			apiError(t, request(t, f.handler, verb, path, `{}`, cookie, "", publicURL), 403, "forbidden")
		}
	}
	for _, body := range []string{
		`{"categories":["audit_events","invalid"],"keep_days":30,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`,
		`{"categories":["audit_events","audit_events"],"keep_days":30,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`,
		`{"categories":["audit_events"],"keep_days":0,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`,
		`{"categories":["audit_events"],"keep_days":31,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`,
		`{"categories":["audit_events"],"keep_days":30,"confirmation":"clean","before":"2026-01-01T00:00:00Z"}`,
		`{"categories":[],"keep_days":30,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`,
		`{"categories":["metric_history"],"keep_days":30,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`,
		`{"categories":["audit_events"],"keep_days":30,"confirmation":"CLEAN"}`,
		`{"categories":["audit_events"],"keep_days":30,"confirmation":"CLEAN","before":"0001-01-01T00:00:00Z"}`,
		`{"categories":["audit_events"],"keep_days":30,"confirmation":"CLEAN","before":"2999-01-01T00:00:00Z"}`,
	} {
		apiError(t, request(t, f.handler, "POST", "/api/v1/data-management/cleanup", body, cookie, csrf, publicURL), 400, "validation_failed")
	}
	for _, query := range []string{"?keep_days=0", "?keep_days=no", "?keep_days=30&keep_days=7"} {
		apiError(t, request(t, f.handler, "GET", "/api/v1/data-management"+query, "", cookie, "", ""), 400, "validation_failed")
	}
	for _, body := range []string{`{"domain":"other","confirmation":"COMPACT"}`, `{"domain":"main","confirmation":"compact"}`, `{"domain":"history","confirmation":"COMPACT"}`} {
		apiError(t, request(t, f.handler, "POST", "/api/v1/data-management/compact", body, cookie, csrf, publicURL), 400, "validation_failed")
	}
	var count int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE id='protected-old'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("validation mutated data: %d %v", count, err)
	}
}

func TestDataManagementCleanupBoundAndAuditRollback(t *testing.T) {
	f := newHTTP(t, 3)
	cookie, csrf := login(t, f, "admin")
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	statement, err := tx.Prepare("INSERT INTO audit_events(id,action,target_id,created_at,details) VALUES(?,'fixture','','2000-01-01T00:00:00.000000000Z','{}')")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5001 {
		if _, err = statement.Exec(fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err = statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	body := `{"categories":["audit_events"],"keep_days":30,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`
	for _, want := range []struct {
		n    int64
		more bool
	}{{5000, true}, {1, false}} {
		response := request(t, f.handler, "POST", "/api/v1/data-management/cleanup", body, cookie, csrf, publicURL)
		assertHTTPStatus(t, response, 200)
		var result struct {
			Results []struct {
				Deleted int64 `json:"deleted_rows"`
				More    bool  `json:"has_more"`
			} `json:"results"`
		}
		if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Results) != 1 || result.Results[0].Deleted != want.n || result.Results[0].More != want.more {
			t.Fatalf("bounded result: %+v", result)
		}
	}
	if _, err = f.db.Exec("INSERT INTO audit_events(id,action,target_id,created_at,details) VALUES('rollback','fixture','','2000-01-01T00:00:00.000000000Z','{}'); CREATE TRIGGER fail_data_audit BEFORE INSERT ON audit_events WHEN NEW.action='data.cleanup' BEGIN SELECT RAISE(ABORT,'sensitive failure'); END"); err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, "POST", "/api/v1/data-management/cleanup", body, cookie, csrf, publicURL)
	assertHTTPStatus(t, response, 500)
	if strings.Contains(response.Body.String(), "sensitive failure") {
		t.Fatal("database details leaked")
	}
	var count int
	if err = f.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE id='rollback'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit failure lost data: %d %v", count, err)
	}
}

func dataManagementHTTP(t *testing.T, dbs *storage.Databases) fixture {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }
	a := auth.NewService(dbs.Main, auth.Options{Now: now})
	if _, err := a.CreateUser(t.Context(), "admin", password, false); err != nil {
		t.Fatal(err)
	}
	n := nodes.NewService(dbs.Main, nodes.Options{Now: now, MaxNodes: 3})
	h, err := server.NewHandler(testHandlerOptions(t, server.Options{Storage: dbs, Auth: a, Nodes: n, PublicURL: publicURL, Now: now}))
	if err != nil {
		t.Fatal(err)
	}
	return fixture{handler: h, auth: a, nodes: n}
}

func TestDataManagementHistoryAndMySQLParity(t *testing.T) {
	for _, driver := range []string{"sqlite", "mysql"} {
		t.Run(driver, func(t *testing.T) {
			var dbs storage.Databases
			if driver == "mysql" {
				native := storagetest.Start(t)
				dbs.Main = native.Open(t, storage.MainDomain)
				dbs.History = native.Open(t, storage.HistoryDomain)
			} else {
				dir := t.TempDir()
				if err := os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
				var err error
				dbs.Main, err = storage.OpenMain(t.Context(), filepath.Join(dir, "main.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { dbs.Main.Close() })
				dbs.History, err = storage.OpenHistory(t.Context(), filepath.Join(dir, "history.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { dbs.History.Close() })
			}
			f := dataManagementHTTP(t, &dbs)
			cookie, csrf := login(t, f, "admin")
			dataManagementTerminalAndExcludedRows(t, f, &dbs)
			lease, err := dbs.History.Acquire(t.Context(), storage.Writer)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := lease.BeginWrite(lease.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, stamp := range []int64{1, 4102444800} {
				if _, err = tx.ExecContext(lease.Context(), "INSERT INTO buckets(node_id,metric,step,t,total,peak,samples) VALUES('n','cpu',60,?,10,10,1)", stamp); err != nil {
					t.Fatal(err)
				}
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			lease.Close()
			response := request(t, f.handler, "GET", "/api/v1/data-management?keep_days=30", "", cookie, "", "")
			assertHTTPStatus(t, response, 200)
			var stats struct {
				Databases []struct {
					Driver   string `json:"driver"`
					Reusable *int64 `json:"reusable_bytes"`
					Compact  bool   `json:"compact_supported"`
				} `json:"databases"`
				Categories []struct {
					ID       string `json:"id"`
					Rows     int64  `json:"rows"`
					Eligible int64  `json:"eligible_rows"`
				} `json:"categories"`
			}
			if err = json.Unmarshal(response.Body.Bytes(), &stats); err != nil {
				t.Fatal(err)
			}
			if len(stats.Databases) != 2 {
				t.Fatalf("missing databases: %s", response.Body)
			}
			for _, db := range stats.Databases {
				if db.Driver != driver || db.Compact != (driver == "sqlite") || (db.Reusable == nil) != (driver == "mysql") {
					t.Fatalf("incorrect database capabilities: %+v", db)
				}
			}
			found := false
			for _, category := range stats.Categories {
				if category.ID == "metric_history" {
					found = true
					if category.Rows != 2 || category.Eligible != 1 {
						t.Fatalf("history stats mismatch: %+v", category)
					}
				}
			}
			if !found {
				t.Fatal("missing history category")
			}
			response = request(t, f.handler, "POST", "/api/v1/data-management/cleanup", `{"categories":["metric_history"],"keep_days":30,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`, cookie, csrf, publicURL)
			assertHTTPStatus(t, response, 200)
			reader, err := dbs.History.Acquire(t.Context(), storage.Reader)
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err = reader.QueryRowContext(reader.Context(), "SELECT COUNT(*) FROM buckets WHERE t=4102444800").Scan(&count); err != nil || count != 1 {
				t.Fatalf("live history lost: %d %v", count, err)
			}
			if err = reader.QueryRowContext(reader.Context(), "SELECT COUNT(*) FROM buckets WHERE t=1").Scan(&count); err != nil || count != 0 {
				t.Fatalf("old history retained: %d %v", count, err)
			}
			reader.Close()
			main, err := dbs.Main.Acquire(t.Context(), storage.Reader)
			if err != nil {
				t.Fatal(err)
			}
			if err = main.QueryRowContext(main.Context(), "SELECT COUNT(*) FROM audit_events WHERE target_id='metric_history' AND action IN ('data.cleanup_intent','data.cleanup_complete')").Scan(&count); err != nil || count != 2 {
				t.Fatalf("history audit missing: %d %v", count, err)
			}
			main.Close()
			if driver == "mysql" {
				response = request(t, f.handler, "POST", "/api/v1/data-management/compact", `{"domain":"main","confirmation":"COMPACT"}`, cookie, csrf, publicURL)
				apiError(t, response, 400, "validation_failed")
			}
			if driver == "sqlite" {
				response = request(t, f.handler, "POST", "/api/v1/data-management/compact", `{"domain":"history","confirmation":"COMPACT"}`, cookie, csrf, publicURL)
				assertHTTPStatus(t, response, 200)
			}
		})
	}
}

func TestDataManagementUsesConfirmedCutoffWithoutAgingIntoDeletion(t *testing.T) {
	f := newHTTP(t, 3)
	cookie, csrf := login(t, f, "admin")
	for _, row := range []struct{ id, stamp string }{{"reviewed-old", "2000-01-01T00:00:00.000000000Z"}, {"not-reviewed", "2026-02-01T00:00:00.000000000Z"}} {
		if _, err := f.db.Exec("INSERT INTO audit_events(id,action,target_id,created_at,details) VALUES(?,'fixture','',?,'{}')", row.id, row.stamp); err != nil {
			t.Fatal(err)
		}
	}
	response := request(t, f.handler, "POST", "/api/v1/data-management/cleanup", `{"categories":["audit_events"],"keep_days":30,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`, cookie, csrf, publicURL)
	assertHTTPStatus(t, response, 200)
	var result struct {
		Before string `json:"before"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Before != "2026-01-01T00:00:00Z" {
		t.Fatalf("cutoff advanced: %s", result.Before)
	}
	var count int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE id='not-reviewed'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("unreviewed row deleted: %d %v", count, err)
	}
}

func TestDataManagementEarlierBatchesRemainCommittedOnLaterFailure(t *testing.T) {
	f := newHTTP(t, 3)
	cookie, csrf := login(t, f, "admin")
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 129 {
		if _, err = tx.Exec("INSERT INTO audit_events(id,action,target_id,created_at,details) VALUES(?,'fixture','','2000-01-01T00:00:00.000000000Z','{}')", fmt.Sprintf("partial-%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec("CREATE TRIGGER fail_later_batch BEFORE DELETE ON audit_events WHEN OLD.id='partial-128' BEGIN SELECT RAISE(ABORT,'sensitive failure'); END"); err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, "POST", "/api/v1/data-management/cleanup", `{"categories":["audit_events"],"keep_days":30,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`, cookie, csrf, publicURL)
	assertHTTPStatus(t, response, 500)
	if strings.Contains(response.Body.String(), "sensitive failure") {
		t.Fatal("database details leaked")
	}
	var count int
	if err = f.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE action='fixture'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("previous bounded commits lost: count=%d err=%v", count, err)
	}
	if err = f.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE action='data.cleanup'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("committed batch missing audit: %d %v", count, err)
	}
}
