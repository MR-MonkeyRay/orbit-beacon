package server_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/commands"
	"orbitbeacon/internal/server/nodes"
	"testing"
)

func TestCommandAPIValidationAuthenticationCSRFAndLifecycle(t *testing.T) {
	f := newHTTP(t, 2)
	n, err := f.nodes.CreatePending(t.Context(), nodes.CreateRequest{Name: "host"})
	if err != nil {
		t.Fatal(err)
	}
	runtime, _ := json.Marshal(v1.DefaultRuntimeSettings())
	hash := sha256.Sum256([]byte("agent credential"))
	if _, err := f.db.Exec(`UPDATE nodes SET token_hash=?,agent_runtime=?,run_generation='1',report_session_id='session',status='unknown' WHERE id=?`, hash[:], string(runtime), n.ID); err != nil {
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
