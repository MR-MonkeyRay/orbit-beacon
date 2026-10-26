package server_test

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"
)

// Capture persisted state, including credentials and related rows, rather than
// assuming the handler's error proves that an earlier write was rolled back.
func managementState(t *testing.T, f fixture) map[string][]string {
	t.Helper()
	state := make(map[string][]string)
	for _, table := range []string{"users", "sessions", "groups", "nodes", "node_tags", "enrollment_tokens", "enrollment_claims", "update_jobs", "update_job_nodes", "audit_events"} {
		state[table] = nil
		rows, err := f.db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			data, err := json.Marshal(values)
			if err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			state[table] = append(state[table], string(data))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(state[table])
	}
	return state
}

func TestAdministrativeMutationsAndAuditInsertCommitOrRollbackTogether(t *testing.T) {
	const nodeID = "00000000-0000-4000-8000-000000000111"
	const groupID = "00000000-0000-4000-8000-000000000222"
	const userID = "00000000-0000-4000-8000-000000000333"
	const jobID = "00000000-0000-4000-8000-000000000444"
	for _, mutation := range []struct {
		name   string
		method string
		path   string
		body   string
		target string
		status int
		batch  bool
	}{
		{"create node", http.MethodPost, "/api/v1/nodes", `{"name":"created with audit"}`, "", http.StatusCreated, false},
		{"patch node metadata and tags", http.MethodPatch, "/api/v1/nodes/" + nodeID, `{"notes":"new note","region":"eu","provider":"cloud","group_id":"` + groupID + `","tags":["new"]}`, nodeID, http.StatusOK, false},
		{"patch desired configuration", http.MethodPatch, "/api/v1/nodes/" + nodeID + "/config", `{"report_interval_seconds":30}`, nodeID, http.StatusOK, false},
		{"delete node and revoke identity", http.MethodDelete, "/api/v1/nodes/" + nodeID + "?confirm=true", "", nodeID, http.StatusNoContent, false},
		{"batch disable node", http.MethodPost, "/api/v1/nodes/batch", `{"node_ids":["` + nodeID + `"],"action":"disable"}`, nodeID, http.StatusOK, true},
		{"create group", http.MethodPost, "/api/v1/groups", `{"name":"created with audit"}`, "", http.StatusCreated, false},
		{"patch group", http.MethodPatch, "/api/v1/groups/" + groupID, `{"name":"renamed with audit"}`, groupID, http.StatusOK, false},
		{"delete group and detach nodes", http.MethodDelete, "/api/v1/groups/" + groupID, "", groupID, http.StatusNoContent, false},
		{"create user", http.MethodPost, "/api/v1/users", `{"username":"created-with-audit","password":"` + password + `","role":"viewer"}`, "", http.StatusCreated, false},
		{"patch user", http.MethodPatch, "/api/v1/users/" + userID, `{"username":"renamed-with-audit","role":"admin"}`, userID, http.StatusOK, false},
		{"delete user and revoke sessions", http.MethodDelete, "/api/v1/users/" + userID, "", userID, http.StatusNoContent, false},
		{"change password and revoke sessions", http.MethodPatch, "/api/v1/me/password", `{"old_password":"` + password + `","new_password":"replacement regression password"}`, "", http.StatusNoContent, false},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			for _, auditFails := range []bool{false, true} {
				name := "commits with audit"
				if auditFails {
					name = "rolls back when audit fails"
				}
				t.Run(name, func(t *testing.T) {
					f := newHTTP(t, 10)
					cookie, csrf := login(t, f, "admin")
					// Use real schema and independent seed rows, so a missing management
					// route does not prevent the other mutation cases from running.
					var actorID, hash string
					if err := f.db.QueryRow("SELECT id,password_hash FROM users WHERE username='admin'").Scan(&actorID, &hash); err != nil {
						t.Fatal(err)
					}
					now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
					for _, seed := range []struct {
						query string
						args  []any
					}{
						{"INSERT INTO groups(id,name) VALUES (?,?)", []any{groupID, "original group"}},
						{"INSERT INTO users(id,username,password_hash,role,created_at) VALUES (?,?,?,?,?)", []any{userID, "target-user", hash, "viewer", now.Format(time.RFC3339Nano)}},
						{"INSERT INTO nodes(id,name,group_id,notes,created_at) VALUES (?,?,?,?,?)", []any{nodeID, "original node", groupID, "original notes", now.Format(time.RFC3339Nano)}},
						{"INSERT INTO node_tags(node_id,tag) VALUES (?,?)", []any{nodeID, "original"}},
						{"INSERT INTO update_jobs(id,release_seq,created_at) VALUES (?,?,?)", []any{jobID, 1, now.Format(time.RFC3339Nano)}},
						{"INSERT INTO update_job_nodes(job_id,node_id,status) VALUES (?,?,?)", []any{jobID, nodeID, "queued"}},
					} {
						if _, err := f.db.Exec(seed.query, seed.args...); err != nil {
							t.Fatal(err)
						}
					}
					digest := sha256.Sum256([]byte("secret credential must survive rollback"))
					if _, err := f.db.Exec("UPDATE nodes SET token_hash=? WHERE id=?", digest[:], nodeID); err != nil {
						t.Fatal(err)
					}
					if _, err := f.db.Exec("INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES (?,?,?,?)", digest[:], userID, digest[:], now.Add(time.Hour).UnixNano()); err != nil {
						t.Fatal(err)
					}
					if auditFails {
						if _, err := f.db.Exec(`CREATE TRIGGER reject_regression_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'regression audit unavailable'); END`); err != nil {
							t.Fatal(err)
						}
					}
					before := managementState(t, f)
					response := request(t, f.handler, mutation.method, mutation.path, mutation.body, cookie, csrf, publicURL)
					if auditFails {
						if mutation.batch && response.Code == http.StatusOK {
							object := decodeObject(t, response)
							results, ok := object["results"].([]any)
							if !ok || len(results) != 1 {
								t.Fatalf("failed audit lacks per-node batch result: %+v", object)
							}
							result, ok := results[0].(map[string]any)
							if !ok || result["node_id"] != nodeID || result["status"] != "failed" || result["error_code"] == nil || result["error_code"] == "" {
								t.Errorf("failed audit reported successful batch mutation: %+v", results[0])
							}
						} else {
							apiError(t, response, http.StatusInternalServerError, "internal_error")
						}
						after := managementState(t, f)
						for table, rows := range before {
							if !reflect.DeepEqual(rows, after[table]) {
								t.Errorf("audit failure changed persisted %s state", table)
							}
						}
						return
					}
					assertHTTPStatus(t, response, mutation.status)
					target := mutation.target
					if mutation.status == http.StatusCreated {
						target = objectID(t, decodeObject(t, response))
					} else if mutation.path == "/api/v1/me/password" {
						target = actorID
					}
					var count int
					if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE actor_id=? AND target_id=? AND action <> ''", actorID, target).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count == 0 {
						t.Error("successful mutation omitted its attributed audit event")
					}
					rows, err := f.db.Query("SELECT details FROM audit_events WHERE actor_id=? AND target_id=?", actorID, target)
					if err != nil {
						t.Fatal(err)
					}
					for rows.Next() {
						var details string
						if err := rows.Scan(&details); err != nil {
							_ = rows.Close()
							t.Fatal(err)
						}
						var decoded any
						if err := json.Unmarshal([]byte(details), &decoded); err != nil {
							_ = rows.Close()
							t.Fatal(err)
						}
						assertNoCredentialDTO(t, decoded)
					}
					if err := rows.Err(); err != nil {
						_ = rows.Close()
						t.Fatal(err)
					}
					if err := rows.Close(); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}
