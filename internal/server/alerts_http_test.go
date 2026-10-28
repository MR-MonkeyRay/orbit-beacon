package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

const validAlertRule = `{"name":"CPU hot","enabled":true,"scope_kind":"all","scope_id":null,"metric":"cpu_percent","comparator":">","threshold":80,"for_seconds":60,"severity":"warning"}`

func TestAlertHTTPCRUDValidationAndAuthz(t *testing.T) {
	f := newHTTP(t, 3)
	admin, csrf := login(t, f, "admin")
	second, secondCSRF := login(t, f, "admin-two")
	response := request(t, f.handler, "POST", "/api/v1/alert-rules", validAlertRule, second, secondCSRF, publicURL)
	assertHTTPStatus(t, response, http.StatusCreated)
	object := decodeObject(t, response)
	id := objectID(t, object)
	path := "/api/v1/alert-rules/" + id
	collectionContains(t, request(t, f.handler, "GET", "/api/v1/alert-rules", "", second, "", ""), "rules", id)
	assertHTTPStatus(t, request(t, f.handler, "GET", path, "", second, "", ""), 200)
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"name":"renamed","threshold":85}`, second, secondCSRF, publicURL), 200)
	for _, body := range []string{`{}`, `{"name":null}`, `{"extra":1}`, strings.Replace(validAlertRule, `"threshold":80`, `"threshold":101`, 1), strings.Replace(validAlertRule, `"threshold":80`, `"threshold":1e999`, 1), strings.Replace(validAlertRule, `"for_seconds":60`, `"for_seconds":86401`, 1), strings.Replace(validAlertRule, `"scope_kind":"all","scope_id":null`, `"scope_kind":"node","scope_id":"00000000-0000-4000-8000-000000000099"`, 1)} {
		assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/alert-rules", body, admin, csrf, publicURL), 400)
	}
	for _, body := range []string{`{"enabled":null}`, `{"scope_kind":"node"}`, `{"threshold":null}`, `{"unknown":true}`, `{"metric":"offline","threshold":null,"comparator":null,"for_seconds":179}`} {
		assertHTTPStatus(t, request(t, f.handler, "PATCH", path, body, admin, csrf, publicURL), 400)
	}
	for _, test := range []struct{ method, path, body string }{{"POST", "/api/v1/alert-rules", validAlertRule}, {"PATCH", path, `{"enabled":false}`}, {"DELETE", path, ""}} {
		assertHTTPStatus(t, request(t, f.handler, test.method, test.path, test.body, second, csrf, publicURL), 403)
		assertHTTPStatus(t, request(t, f.handler, test.method, test.path, test.body, admin, "", publicURL), 403)
		assertHTTPStatus(t, request(t, f.handler, test.method, test.path, test.body, admin, csrf, "https://foreign.test"), 403)
	}
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"metric":"offline","threshold":null,"comparator":null,"for_seconds":180}`, second, secondCSRF, publicURL), 200)
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/alerts", "", second, "", ""), 200)
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/alerts", "", nil, "", ""), 401)
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/alerts/summary", "", second, "", ""), 200)
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/alerts/summary", "", nil, "", ""), 401)
	assertHTTPStatus(t, request(t, f.handler, "DELETE", path, "", second, secondCSRF, publicURL), 204)
	assertHTTPStatus(t, request(t, f.handler, "GET", path, "", second, "", ""), 404)
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE target_id=?", id).Scan(&count); err != nil || count != 4 {
		t.Fatalf("rule audits=%d err=%v", count, err)
	}
}
func TestAlertHTTP405AndQueryValidation(t *testing.T) {
	f := newHTTP(t, 1)
	second, _ := login(t, f, "admin-two")
	for _, tt := range []struct{ path, method, allow string }{{"/api/v1/alert-rules", "PUT", "GET, POST"}, {"/api/v1/alert-rules/00000000-0000-4000-8000-000000000001", "POST", "GET, PATCH, DELETE"}, {"/api/v1/alerts", "POST", "GET"}, {"/api/v1/alerts/summary", "POST", "GET"}} {
		r := request(t, f.handler, tt.method, tt.path, "", nil, "", "")
		assertHTTPStatus(t, r, 405)
		if got := r.Header().Get("Allow"); got != tt.allow {
			t.Fatalf("Allow=%s want %s", got, tt.allow)
		}
	}
	for _, query := range []string{"state=bad", "state=", "state=all&state=firing", "limit=0", "limit=101", "limit=no", "limit=", "before=bad", "before=", "offset=0", "unknown=x", "%zz=x"} {
		assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/alerts?"+query, "", second, "", ""), 400)
	}
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/alert-rules/not-a-uuid", "", second, "", ""), 404)
}

func TestAlertHTTPSummaryEnvelope(t *testing.T) {
	f := newHTTP(t, 1)
	second, _ := login(t, f, "admin-two")
	stamp := timekey.Format(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	if _, err := f.db.Exec(`INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,rule_name,node_name,rule_snapshot)
 VALUES('00000000-0000-4000-8000-000000000001','deleted-rule','deleted-node','firing',90,?,'retained rule','retained node','{"metric":"cpu_percent","severity":"critical"}')`, stamp); err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, "GET", "/api/v1/alerts/summary", "", second, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	var summary alerts.AlertSummary
	if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Firing != (alerts.FiringCounts{Total: 1, Critical: 1}) || len(summary.Newest) != 1 || summary.Newest[0].Metric != "cpu_percent" || summary.Newest[0].RuleName != "retained rule" {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestAlertHTTPHistoryEnvelopeAndRetentionLoop(t *testing.T) {
	f := newHTTP(t, 1)
	second, _ := login(t, f, "admin-two")
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003"} {
		_, err := f.db.Exec(`INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,resolved_at,rule_name,node_name,rule_snapshot) VALUES(?,'deleted-rule','deleted-node','resolved',NULL,?,?,'retained rule','retained node','{}')`, id, timekey.Format(now.Add(time.Duration(i)*time.Second)), timekey.Format(now.Add(-100*24*time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
	}
	response := request(t, f.handler, "GET", "/api/v1/alerts?state=resolved&limit=2", "", second, "", "")
	assertHTTPStatus(t, response, 200)
	var page alerts.EventPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || !page.HasMore || page.NextCursor == "" || page.Events[0].RuleName != "retained rule" || page.Events[0].NodeName != "retained node" {
		t.Fatalf("page=%+v", page)
	}
	response = request(t, f.handler, "GET", "/api/v1/alerts?state=resolved&limit=2&before="+page.NextCursor, "", second, "", "")
	assertHTTPStatus(t, response, 200)
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.HasMore {
		t.Fatalf("next=%+v", page)
	}
	cleaner, ok := f.handler.(interface {
		CleanupRetention(context.Context, time.Time) (server.RetentionResult, error)
	})
	if !ok {
		t.Fatal("missing cleanup")
	}
	result, err := cleaner.CleanupRetention(context.Background(), now)
	if err != nil || result.AlertEvents != 3 {
		t.Fatalf("cleanup=%+v %v", result, err)
	}
}

func TestAlertScopedCascadeAuditRollsBackWithParent(t *testing.T) {
	for _, kind := range []string{"node", "group"} {
		t.Run(kind, func(t *testing.T) {
			f := newHTTP(t, 2)
			admin, csrf := login(t, f, "admin")
			var scopeID, path string
			if kind == "group" {
				scopeID = createHTTPGroup(t, f, admin, csrf, "alert group")
				path = "/api/v1/groups/" + scopeID
			} else {
				n, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "scope"})
				if err != nil {
					t.Fatal(err)
				}
				scopeID = n.ID
				path = "/api/v1/nodes/" + scopeID + "?confirm=true"
			}
			body := strings.Replace(validAlertRule, `"scope_kind":"all","scope_id":null`, `"scope_kind":"`+kind+`","scope_id":"`+scopeID+`"`, 1)
			response := request(t, f.handler, "POST", "/api/v1/alert-rules", body, admin, csrf, publicURL)
			assertHTTPStatus(t, response, 201)
			ruleID := objectID(t, decodeObject(t, response))
			if _, err := f.db.Exec(`CREATE TRIGGER reject_rule_delete_audit BEFORE INSERT ON audit_events WHEN NEW.action='alert_rule.delete' BEGIN SELECT RAISE(ABORT,'audit failure'); END`); err != nil {
				t.Fatal(err)
			}
			assertHTTPStatus(t, request(t, f.handler, "DELETE", path, "", admin, csrf, publicURL), 500)
			assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/alert-rules/"+ruleID, "", admin, "", ""), 200)
			if _, err := f.db.Exec("DROP TRIGGER reject_rule_delete_audit"); err != nil {
				t.Fatal(err)
			}
			assertHTTPStatus(t, request(t, f.handler, "DELETE", path, "", admin, csrf, publicURL), 204)
			assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/alert-rules/"+ruleID, "", admin, "", ""), 404)
			var count int
			if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE action='alert_rule.delete' AND target_id=?", ruleID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("cascade audit=%d %v", count, err)
			}
		})
	}
}
