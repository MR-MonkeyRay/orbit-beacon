package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func TestRuntimeOwnershipHTTPAndTrafficSettings(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "local runtime"})
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/nodes/" + node.ID
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, base+"/config", "", admin, "", ""), http.StatusNotFound)
	observed := request(t, f.handler, http.MethodGet, base+"/runtime", "", admin, "", "")
	assertHTTPStatus(t, observed, http.StatusOK)
	if strings.TrimSpace(observed.Body.String()) != `{"runtime":null}` {
		t.Fatalf("unreported runtime=%s", observed.Body.String())
	}
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/nodes/00000000-0000-4000-8000-000000000999/runtime", "", admin, "", ""), http.StatusNotFound, "node_not_found")
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, base+"/config", `{"billing_day":2}`, admin, csrf, publicURL), http.StatusNotFound)
	path := base + "/traffic-settings"
	before := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
	assertHTTPStatus(t, before, http.StatusOK)
	for _, body := range []string{`{"billing_day":null}`, `{"quota_bytes":null}`, `{"billing_day":0}`, `{"quota_bytes":"01"}`, `{"quota_bytes":2}`, `{"report_interval_seconds":5}`} {
		response := request(t, f.handler, http.MethodPatch, path, body, admin, csrf, publicURL)
		assertHTTPStatus(t, response, http.StatusBadRequest)
		after := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
		if before.Body.String() != after.Body.String() {
			t.Fatalf("invalid patch changed settings: %s", body)
		}
	}
	for range 2 {
		changed := request(t, f.handler, http.MethodPatch, path, `{"billing_day":2,"quota_bytes":"1000000000"}`, admin, csrf, publicURL)
		assertHTTPStatus(t, changed, http.StatusOK)
		var values map[string]any
		if err := json.Unmarshal(changed.Body.Bytes(), &values); err != nil || len(values) != 2 || values["billing_day"] != float64(2) || values["quota_bytes"] != "1000000000" {
			t.Fatalf("traffic settings=%v err=%v", values, err)
		}
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, path, `{}`, admin, csrf, publicURL), http.StatusOK)
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, path, `{"billing_day":2}`, admin, csrf, publicURL), http.StatusOK)
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='node.traffic_settings.patch' AND target_id=?`, node.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit count=%d err=%v", count, err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_traffic_settings BEFORE INSERT ON audit_events WHEN NEW.action='node.traffic_settings.patch' BEGIN SELECT RAISE(ABORT,'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, path, `{"billing_day":3}`, admin, csrf, publicURL), http.StatusInternalServerError)
	current := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
	if !strings.Contains(current.Body.String(), `"billing_day":2`) {
		t.Fatalf("failed audit committed settings: %s", current.Body.String())
	}
}

func TestObservedRuntimeCannotRollbackOnDuplicateAndRevokeClearsIdentity(t *testing.T) {
	f := newAgentHTTP(t, 2)
	id, token, _ := enrollHTTP(t, f, "runtime-owner", 1)
	withRuntime := func(seq uint64, targets string) string {
		return strings.Replace(reportBody(1, seq, "runtime-session", 25), `"runtime":{"report_interval_seconds":10,"disk_interval_seconds":60,"interfaces":[],"disk_paths":[],"probe_targets":[]}`, `"runtime":{"report_interval_seconds":5,"disk_interval_seconds":60,"interfaces":[],"disk_paths":[],"probe_targets":`+targets+`}`, 1)
	}
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, withRuntime(1, `["tcp://old.test:443"]`)), 1)
	checked := "2026-10-26T12:00:00.000000000Z"
	if _, err := f.db.Exec(`INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at) VALUES(?,?,1,1,'',NULL,?)`, id, "tcp://old.test:443", checked); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES(?,?,?,1,0,1)`, id, "tcp://old.test:443", checked); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, withRuntime(2, `[]`)), 2)
	f.now = f.now.Add(time.Second)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, withRuntime(1, `["tcp://old.test:443"]`)), 2)
	var raw string
	if err := f.db.QueryRow(`SELECT agent_runtime FROM nodes WHERE id=?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "old.test") || !strings.Contains(raw, `"report_interval_seconds":5`) {
		t.Fatalf("rolled-back runtime=%s", raw)
	}
	for _, table := range []string{"probe_latest", "probe_hourly"} {
		var count int
		if err := f.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE node_id=?`, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained removed target count=%d err=%v", table, count, err)
		}
	}
	if err := f.nodes.RevokeCredential(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var system, generation, session string
	var seen any
	if err := f.db.QueryRow(`SELECT agent_runtime,last_seen,system,run_generation,report_session_id FROM nodes WHERE id=?`, id).Scan(&raw, &seen, &system, &generation, &session); err != nil {
		t.Fatal(err)
	}
	if raw != "" || seen != nil || system != "{}" || generation != "0" || session != "" {
		t.Fatalf("revoked observation leaked: %q %v %q %q %q", raw, seen, system, generation, session)
	}
	if _, err := f.ingest.Latest(context.Background(), id); err == nil {
		t.Fatal("old latest survived revocation")
	}
}

func TestCredentialReplacementClearsObservedIdentityButIdempotentEnrollPreservesIt(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "replace-runtime", 1)
	body := strings.Replace(reportBody(4, 1, "old-session", 25), `"traffic":[]`, `"traffic":[],"system":{"hostname":"old-host","os":"linux","arch":"amd64","agent_version":"old-version"}`, 1)
	body = strings.Replace(body, `"probe_targets":[]`, `"probe_targets":["tcp://identity.test:443"]`, 1)
	body = strings.Replace(body, `"traffic":[]`, `"probe_results":[{"target":"tcp://identity.test:443","ok":true,"latency_ms":1,"error":"","status_code":null,"checked_at":"2026-10-26T12:00:00Z"}],"traffic":[]`, 1)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, body), 1)
	notes := "retained notes"
	if _, err := f.nodes.PatchMetadata(context.Background(), id, nodes.MetadataPatch{Notes: &notes}); err != nil {
		t.Fatal(err)
	}
	enabled, publicName := true, "Retained public name"
	if _, err := f.nodes.PatchPublicSettings(context.Background(), id, nodes.PublicSettingsPatch{Enabled: &enabled, Name: &publicName}); err != nil {
		t.Fatal(err)
	}
	day, quota := 2, "123456789"
	if _, err := f.nodes.PatchTrafficSettings(context.Background(), id, nodes.TrafficSettingsPatch{BillingDay: &day, QuotaBytes: &quota}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO traffic_daily(node_id,day,rx_bytes,tx_bytes) VALUES(?,?,123,456)`, id, "2026-10-28"); err != nil {
		t.Fatal(err)
	}
	first, err := f.nodes.Get(context.Background(), id)
	if err != nil || first.LastSeen == nil || first.AgentVersion != "old-version" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	var enrolledAt string
	if err := f.db.QueryRow(`SELECT first_enrolled_at FROM nodes WHERE id=?`, id).Scan(&enrolledAt); err != nil {
		t.Fatal(err)
	}
	settings := func() string {
		var raw string
		if err := f.db.QueryRow(`SELECT agent_runtime FROM nodes WHERE id=?`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := settings()
	var epochBefore int64
	if err := f.db.QueryRow(`SELECT credential_epoch FROM nodes WHERE id=?`, id).Scan(&epochBefore); err != nil || epochBefore < 1 {
		t.Fatalf("initial credential epoch=%d err=%v", epochBefore, err)
	}
	authenticated, err := f.nodes.AuthenticateToken(context.Background(), token)
	if err != nil || authenticated.ID != id || authenticated.CredentialEpoch != epochBefore {
		t.Fatalf("authenticated identity=%+v epoch=%d err=%v", authenticated, epochBefore, err)
	}
	ticket, err := f.enrollments.CreateSingle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, enrollBody("installation-replace-runtime", token)), http.StatusOK)
	same, err := f.nodes.Get(context.Background(), id)
	if err != nil || same.LastSeen == nil || !same.LastSeen.Equal(*first.LastSeen) || same.AgentVersion != "old-version" || settings() != before {
		t.Fatalf("same credential erased observations: %+v err=%v", same, err)
	}
	var epochSame int64
	if err := f.db.QueryRow(`SELECT credential_epoch FROM nodes WHERE id=?`, id).Scan(&epochSame); err != nil || epochSame != epochBefore {
		t.Fatalf("idempotent enrollment changed epoch: before=%d after=%d err=%v", epochBefore, epochSame, err)
	}
	ticket, err = f.enrollments.CreateSingle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	replacement := agentSecret(2)
	for _, table := range []string{"probe_latest", "probe_hourly"} {
		var count int
		if err := f.db.QueryRow("SELECT count(*) FROM "+table+" WHERE node_id=?", id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("idempotent enrollment erased %s: count=%d err=%v", table, count, err)
		}
	}
	claim := strings.Replace(enrollBody("installation-replace-runtime", replacement), `"system":{"hostname":"identical-host","os":"linux","arch":"amd64"}`, `"previous_node_token":"`+token+`","system":{"hostname":"new-host","os":"linux","arch":"amd64","agent_version":"new-enrollment-version"}`, 1)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_identity_claim BEFORE INSERT ON audit_events WHEN NEW.action='enrollment.claim' BEGIN SELECT RAISE(ABORT,'identity rollback'); END`); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, claim), http.StatusInternalServerError)
	if settings() != before {
		t.Fatal("failed replacement cleared observed runtime")
	}
	var epochFailed int64
	if err := f.db.QueryRow(`SELECT credential_epoch FROM nodes WHERE id=?`, id).Scan(&epochFailed); err != nil || epochFailed != epochBefore {
		t.Fatalf("failed replacement changed epoch: before=%d after=%d err=%v", epochBefore, epochFailed, err)
	}
	for _, table := range []string{"probe_latest", "probe_hourly"} {
		var count int
		if err := f.db.QueryRow("SELECT count(*) FROM "+table+" WHERE node_id=?", id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("failed replacement erased %s: count=%d err=%v", table, count, err)
		}
	}
	if _, err := f.db.Exec(`DROP TRIGGER reject_identity_claim`); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, claim), http.StatusOK)
	var epochReplacement int64
	if err := f.db.QueryRow(`SELECT credential_epoch FROM nodes WHERE id=?`, id).Scan(&epochReplacement); err != nil || epochReplacement != epochBefore+1 {
		t.Fatalf("replacement epoch: before=%d after=%d err=%v", epochBefore, epochReplacement, err)
	}
	current, err := f.nodes.Get(context.Background(), id)
	if err != nil || current.Presence != "never" || current.LastSeen != nil || current.AgentVersion != "new-enrollment-version" || settings() != "" {
		t.Fatalf("replacement observations=%+v runtime=%s err=%v", current, settings(), err)
	}
	if current.Name != "replace-runtime" || current.Notes != notes {
		t.Fatalf("replacement lost metadata: %+v", current)
	}
	public, err := f.nodes.GetPublicSettings(context.Background(), id)
	if err != nil || !public.Enabled || public.Name != publicName {
		t.Fatalf("replacement lost public settings: %+v err=%v", public, err)
	}
	traffic, err := f.nodes.GetTrafficSettings(context.Background(), id)
	if err != nil || traffic.BillingDay != day || traffic.QuotaBytes != quota {
		t.Fatalf("replacement lost traffic settings: %+v err=%v", traffic, err)
	}
	var rx, tx int64
	if err := f.db.QueryRow(`SELECT rx_bytes,tx_bytes FROM traffic_daily WHERE node_id=?`, id).Scan(&rx, &tx); err != nil || rx != 123 || tx != 456 {
		t.Fatalf("replacement lost accumulated traffic=%d/%d err=%v", rx, tx, err)
	}
	var generation, session, firstAgain string
	if err := f.db.QueryRow(`SELECT run_generation,report_session_id,first_enrolled_at FROM nodes WHERE id=?`, id).Scan(&generation, &session, &firstAgain); err != nil || generation != "0" || session != "" || firstAgain != enrolledAt {
		t.Fatalf("replacement fence=%s/%s first=%s err=%v", generation, session, firstAgain, err)
	}
	if _, err := f.ingest.Latest(context.Background(), id); err == nil {
		t.Fatal("replacement exposed old latest")
	}
	for _, table := range []string{"probe_latest", "probe_hourly"} {
		var count int
		if err := f.db.QueryRow("SELECT count(*) FROM "+table+" WHERE node_id=?", id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("replacement leaked %s: count=%d err=%v", table, count, err)
		}
	}
	if err := f.nodes.RevokeCredential(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var epochRevoked int64
	if err := f.db.QueryRow(`SELECT credential_epoch FROM nodes WHERE id=?`, id).Scan(&epochRevoked); err != nil || epochRevoked != epochReplacement+1 {
		t.Fatalf("revoke epoch: before=%d after=%d err=%v", epochReplacement, epochRevoked, err)
	}
	if err := f.nodes.RevokeCredential(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var epochRevokedAgain int64
	if err := f.db.QueryRow(`SELECT credential_epoch FROM nodes WHERE id=?`, id).Scan(&epochRevokedAgain); err != nil || epochRevokedAgain != epochRevoked {
		t.Fatalf("already-revoked identity advanced epoch: before=%d after=%d err=%v", epochRevoked, epochRevokedAgain, err)
	}
}

func TestCredentialEpochExhaustionRollsBackIdentityMutation(t *testing.T) {
	for _, mutation := range []string{"revoke", "replace", "bind"} {
		t.Run(mutation, func(t *testing.T) {
			f := newAgentHTTP(t, 1)
			ctx := context.Background()
			id, token, _ := enrollHTTP(t, f, "epoch-limit", 1)
			accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "epoch-limit", 25)), 1)
			if mutation == "bind" {
				if err := f.nodes.RevokeCredential(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.db.Exec(`UPDATE nodes SET credential_epoch=9223372036854775807 WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
			request := v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "installation-epoch-limit", NodeToken: token, System: v1.System{Hostname: "new-enrollment", OS: "linux", Arch: "amd64"}}
			if mutation != "bind" {
				// Exhaustion does not turn an idempotent same-active-credential
				// enrollment into a mutation or invalidate the active identity.
				ticket, err := f.enrollments.CreateSingle(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.enrollments.Enroll(ctx, ticket.Token, request); err != nil {
					t.Fatalf("idempotent enrollment at maximum epoch: %v", err)
				}
			}
			if mutation == "bind" {
				if err := f.nodes.RevokeCredential(ctx, id); err != nil {
					t.Fatalf("already-revoked credential failed at exhausted epoch: %v", err)
				}
				var epoch int64
				if err := f.db.QueryRow(`SELECT credential_epoch FROM nodes WHERE id=?`, id).Scan(&epoch); err != nil || epoch != 9223372036854775807 {
					t.Fatalf("already-revoked credential consumed epoch: epoch=%d err=%v", epoch, err)
				}
			}
			ticket, err := f.enrollments.CreateSingle(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			capture := func() string {
				t.Helper()
				var raw string
				if err := f.db.QueryRow(`SELECT json_object('epoch',credential_epoch,'type',typeof(credential_epoch),'token',hex(token_hash),'installation',installation_id,'runtime',agent_runtime,'seen',last_seen,'system',system,'generation',run_generation,'session',report_session_id,'claims',(SELECT count(*) FROM enrollment_claims),'uses',(SELECT uses FROM enrollment_tokens WHERE id=?),'audit',(SELECT count(*) FROM audit_events)) FROM nodes WHERE id=?`, ticket.ID, id).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				return raw
			}
			before := capture()
			if mutation == "revoke" {
				err = f.nodes.RevokeCredential(ctx, id)
			} else {
				request.NodeToken = agentSecret(2)
				if mutation == "replace" {
					request.PreviousNodeToken = token
				}
				_, err = f.enrollments.Enroll(ctx, ticket.Token, request)
			}
			if err == nil {
				t.Fatal("exhausted credential epoch allowed an identity mutation")
			}
			if after := capture(); after != before {
				t.Fatalf("exhausted mutation changed identity or consumed enrollment: before=%s after=%s", before, after)
			}
			if mutation != "bind" {
				identity, err := f.nodes.AuthenticateToken(ctx, token)
				if err != nil || identity.CredentialEpoch != 9223372036854775807 {
					t.Fatalf("overflow corrupted active authentication: identity=%+v err=%v", identity, err)
				}
			}
		})
	}
}
