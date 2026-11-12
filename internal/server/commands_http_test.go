package server_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"orbitbeacon/internal/server/commands"
	"orbitbeacon/internal/server/nodes"
)

func TestCommandAPIValidationAuthenticationCSRFAndLifecycle(t *testing.T) {
	f := newHTTP(t, 2)
	n, err := f.nodes.CreatePending(t.Context(), nodes.CreateRequest{Name: "host"})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("agent credential"))
	if _, err := f.db.Exec(`UPDATE nodes SET token_hash=?,run_generation='1',report_session_id='session',status='unknown' WHERE id=?`, hash[:], n.ID); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + n.ID + "/commands"
	cookie, csrf := login(t, f, "admin")
	for _, test := range []struct {
		body   string
		cookie bool
		csrf   string
		want   int
	}{
		{`{"kind":"restart"}`, false, csrf, 401}, {`{"kind":"restart"}`, true, "", 403},
		{`{"kind":"shell"}`, true, csrf, 400}, {`{"kind":"upgrade"}`, true, csrf, 400},
		{`{"kind":"stop","confirm":"wrong"}`, true, csrf, 400}, {`{"kind":"restart","args":"whoami"}`, true, csrf, 400},
	} {
		var c *http.Cookie
		if test.cookie {
			c = cookie
		}
		response := request(t, f.handler, "POST", path, test.body, c, test.csrf, publicURL)
		if response.Code != test.want {
			t.Fatalf("%s: %d %s", test.body, response.Code, response.Body)
		}
	}
	response := request(t, f.handler, "POST", path, `{"kind":"stop","confirm":"host"}`, cookie, csrf, publicURL)
	if response.Code != 201 {
		t.Fatalf("create: %d %s", response.Code, response.Body)
	}
	var created commands.Record
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.RequestedBy != "admin" {
		t.Fatal(created.RequestedBy)
	}
	response = request(t, f.handler, "POST", path, `{"kind":"restart"}`, cookie, csrf, publicURL)
	if response.Code != 409 {
		t.Fatal(response.Code)
	}
	response = request(t, f.handler, "GET", path, "", cookie, "", "")
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	cancel := fmt.Sprintf("%s/%s/cancel", path, created.ID)
	response = request(t, f.handler, "POST", cancel, "", cookie, csrf, publicURL)
	if response.Code != 204 {
		t.Fatalf("cancel: %d %s", response.Code, response.Body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action IN ('command.create','command.canceled')`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}

func TestGroupCommandsConfirmationIsolationAndAudit(t *testing.T) {
	f := newHTTP(t, 200)
	cookie, csrf := login(t, f, "admin")
	groupResponse := request(t, f.handler, "POST", "/api/v1/groups", `{"name":"Production"}`, cookie, csrf, publicURL)
	if groupResponse.Code != 201 {
		t.Fatal(groupResponse.Code, groupResponse.Body)
	}
	var group struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(groupResponse.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		hash := sha256.Sum256([]byte(fmt.Sprintf("credential-%d", i)))
		n, err := f.nodes.CreatePending(t.Context(), nodes.CreateRequest{Name: fmt.Sprintf("host-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`UPDATE nodes SET group_id=?,token_hash=?,run_generation='1',report_session_id='session',status='unknown' WHERE id=?`, group.ID, hash[:], n.ID); err != nil {
			t.Fatal(err)
		}
		switch i {
		case 0:
			response := request(t, f.handler, "POST", "/api/v1/nodes/"+n.ID+"/commands", `{"kind":"restart"}`, cookie, csrf, publicURL)
			if response.Code != 201 {
				t.Fatal(response.Code, response.Body)
			}
		case 1:
			if _, err := f.db.Exec(`UPDATE nodes SET status='disabled' WHERE id=?`, n.ID); err != nil {
				t.Fatal(err)
			}
		case 2:
			if _, err := f.db.Exec(`UPDATE nodes SET run_generation='0' WHERE id=?`, n.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	path := "/api/v1/groups/" + group.ID + "/commands"
	for _, kind := range []string{"restart", "stop"} {
		response := request(t, f.handler, "POST", path, fmt.Sprintf(`{"kind":%q,"confirm":"wrong"}`, kind), cookie, csrf, publicURL)
		if response.Code != 400 {
			t.Fatal(response.Code, response.Body)
		}
	}
	for _, check := range []struct {
		body   string
		cookie *http.Cookie
		csrf   string
		want   int
	}{
		{`{"kind":"restart","confirm":"Production"}`, nil, csrf, 401},
		{`{"kind":"restart","confirm":"Production"}`, cookie, "", 403},
		{`{"kind":"upgrade","confirm":"Production"}`, cookie, csrf, 400},
		{`{"kind":"stop","confirm":"Production","args":[]}`, cookie, csrf, 400},
	} {
		response := request(t, f.handler, "POST", path, check.body, check.cookie, check.csrf, publicURL)
		if response.Code != check.want {
			t.Fatal(response.Code, response.Body)
		}
	}
	missing := request(t, f.handler, "POST", "/api/v1/groups/00000000-0000-4000-8000-999999999999/commands", `{"kind":"restart","confirm":"Production"}`, cookie, csrf, publicURL)
	if missing.Code != 404 {
		t.Fatal(missing.Code, missing.Body)
	}
	response := request(t, f.handler, "POST", path, `{"kind":"stop","confirm":"Production"}`, cookie, csrf, publicURL)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	var result commands.BulkResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Created != 2 || result.Skipped != 3 {
		t.Fatal(result)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM agent_commands WHERE requested_by='admin'`).Scan(&count); err != nil || count != 3 {
		t.Fatal(count, err)
	}
	var details string
	if err := f.db.QueryRow(`SELECT details FROM audit_events WHERE action='command.bulk' AND target_id=?`, group.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		GroupID string `json:"group_id"`
		Kind    string `json:"kind"`
		Created int    `json:"created"`
		Skipped int    `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(details), &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.GroupID != group.ID || recorded.Kind != "stop" || recorded.Created != 2 || recorded.Skipped != 3 {
		t.Fatal(recorded)
	}
}

func TestUpgradeAPIHasOnlyVersionAndSameVersionConflict(t *testing.T) {
	f := newHTTP(t, 2)
	n, err := f.nodes.CreatePending(t.Context(), nodes.CreateRequest{Name: "host"})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("upgrade credential"))
	if _, err := f.db.Exec(`UPDATE nodes SET token_hash=?,run_generation='1',report_session_id='session',status='unknown',system='{"agent_version":"1.2.3"}' WHERE id=?`, hash[:], n.ID); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := login(t, f, "admin")
	path := "/api/v1/nodes/" + n.ID + "/commands"
	for _, body := range []string{`{"kind":"upgrade","target_version":"01.2.3"}`, `{"kind":"upgrade","target_version":"1.2"}`, `{"kind":"restart","target_version":"1.2.4"}`, `{"kind":"restart","target_version":""}`, `{"kind":"stop","target_version":null,"confirm":"host"}`, `{"kind":"stop","target_version":"1.2.4","confirm":"host"}`, `{"kind":"upgrade","target_version":"1.2.4","url":"https://attacker.example"}`, `{"kind":"upgrade","target_version":"1.2.4","hash":"abcd"}`} {
		response := request(t, f.handler, "POST", path, body, cookie, csrf, publicURL)
		if response.Code != 400 {
			t.Fatalf("%s: %d %s", body, response.Code, response.Body)
		}
	}
	same := request(t, f.handler, "POST", path, `{"kind":"upgrade","target_version":"1.2.3"}`, cookie, csrf, publicURL)
	if same.Code != 409 {
		t.Fatal(same.Code, same.Body)
	}
	var failure struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(same.Body.Bytes(), &failure); err != nil || failure.Code != "command_same_version" {
		t.Fatal(failure, err)
	}
	created := request(t, f.handler, "POST", path, `{"kind":"upgrade","target_version":"1.2.4"}`, cookie, csrf, publicURL)
	if created.Code != 201 {
		t.Fatal(created.Code, created.Body)
	}
	var record commands.Record
	if err := json.Unmarshal(created.Body.Bytes(), &record); err != nil || record.TargetVersion != "1.2.4" || record.ExpiresAt.Sub(record.CreatedAt) != commands.UpgradeLifetime {
		t.Fatal(record, err)
	}
}
