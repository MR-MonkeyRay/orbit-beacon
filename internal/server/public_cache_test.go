package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func publishCacheNode(t *testing.T, f *agentFixture) (string, string) {
	t.Helper()
	id, token, _ := enrollHTTP(t, f, "private-cache", 151)
	enabled, name := true, "Public cached"
	if _, err := f.nodes.PatchPublicSettings(context.Background(), id, nodes.PublicSettingsPatch{Enabled: &enabled, Name: &name}); err != nil {
		t.Fatal(err)
	}
	return id, token
}

func TestPublicPageCacheHitAndExpiry(t *testing.T) {
	f := newAgentHTTP(t, 2)
	_, token := publishCacheNode(t, f)
	read := func() (publicListFixture, []byte) {
		return publicList(t, f, "/api/public/v1/nodes?limit=1", nil)
	}
	initial, initialBytes := read()
	if len(initial.Nodes) != 1 || string(initial.Nodes[0]["status"]) != `"unknown"` {
		t.Fatalf("initial page: %s", initialBytes)
	}
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "cache-session", 25)), 1)
	f.now = f.now.Add(2*time.Second - time.Nanosecond)
	hit, hitBytes := read()
	if !reflect.DeepEqual(initial, hit) {
		t.Fatalf("cache did not preserve computed response: %s vs %s", initialBytes, hitBytes)
	}
	f.now = f.now.Add(time.Nanosecond)
	expired, expiredBytes := read()
	if expired.GeneratedAt == initial.GeneratedAt || string(expired.Nodes[0]["status"]) != `"online"` || string(expired.Nodes[0]["cpu_percent"]) != "25" {
		t.Fatalf("cache did not expire at TTL: %s", expiredBytes)
	}
	// Pagination keys must not share a result, including an empty offset page.
	other, _ := publicList(t, f, "/api/public/v1/nodes?limit=1&offset=1", nil)
	if len(other.Nodes) != 0 || other.HasMore || other.NextOffset != nil {
		t.Fatalf("offset page reused first page: %+v", other)
	}
}

func TestPublicPageCacheImmediatelyExcludesWithdrawnNodes(t *testing.T) {
	for _, action := range []string{"unpublish", "disable", "delete", "revoke", "batch-disable", "batch-delete"} {
		t.Run(action, func(t *testing.T) {
			f := newAgentHTTP(t, 2)
			id, _ := publishCacheNode(t, f)
			before, _ := publicList(t, f, "/api/public/v1/nodes?limit=1", nil)
			if len(before.Nodes) != 1 {
				t.Fatalf("missing initial public node: %+v", before)
			}
			ctx := context.Background()
			var err error
			switch action {
			case "unpublish":
				enabled := false
				_, err = f.nodes.PatchPublicSettings(ctx, id, nodes.PublicSettingsPatch{Enabled: &enabled})
			case "disable":
				err = f.nodes.SetDisabled(ctx, id, true)
			case "delete":
				err = f.nodes.Delete(ctx, id, true)
			case "revoke":
				err = f.nodes.RevokeCredential(ctx, id)
			default:
				batchAction := "disable"
				if action == "batch-delete" {
					batchAction = "delete"
				}
				var results []nodes.BatchResult
				results, err = f.nodes.Batch(ctx, nodes.BatchRequest{NodeIDs: []string{id}, Action: batchAction, Confirm: true})
				if err == nil && (len(results) != 1 || results[0].Status != "succeeded") {
					t.Fatalf("batch: %+v", results)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			// Deliberately keep the clock fixed and reuse the populated cache key.
			after, raw := publicList(t, f, "/api/public/v1/nodes?limit=1", nil)
			if len(after.Nodes) != 0 || after.HasMore || after.NextOffset != nil {
				t.Fatalf("withdrawal served cached node: %s", raw)
			}
		})
	}
}

func TestPublicPageCacheImmediatelyUpdatesPublishedText(t *testing.T) {
	f := newAgentHTTP(t, 2)
	id, _ := publishCacheNode(t, f)
	publicList(t, f, "/api/public/v1/nodes", nil)
	name := "Updated public name"
	if _, err := f.nodes.PatchPublicSettings(context.Background(), id, nodes.PublicSettingsPatch{Name: &name}); err != nil {
		t.Fatal(err)
	}
	response := agentHTTPCall(t, f, http.MethodGet, "/api/public/v1/nodes", "", nil, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	if !bytes.Contains(response.Body.Bytes(), []byte(name)) {
		t.Fatalf("published text remained cached: %s", response.Body)
	}
}

func TestPublicPageCacheImmediatelyForgetsRotatedCredentialMetrics(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, oldToken := publishCacheNode(t, f)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", oldToken, reportBody(1, 1, "old-cache-session", 25)), 1)
	before, beforeBytes := publicList(t, f, "/api/public/v1/nodes?limit=1", nil)
	if len(before.Nodes) != 1 || string(before.Nodes[0]["status"]) != `"online"` || string(before.Nodes[0]["cpu_percent"]) != "25" {
		t.Fatalf("initial cached identity: %s", beforeBytes)
	}

	ticket, err := f.enrollments.CreateSingle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(v1.EnrollmentRequest{
		ProtocolVersion:   1,
		InstallationID:    "installation-private-cache",
		NodeToken:         agentSecret(152),
		PreviousNodeToken: oldToken,
		System:            v1.System{Hostname: "rotated-cache", OS: "linux", Arch: "amd64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, string(request)), http.StatusOK)

	// Keep the clock fixed so only identity invalidation can evict the page.
	after, afterBytes := publicList(t, f, "/api/public/v1/nodes?limit=1", nil)
	if len(after.Nodes) != 1 || string(after.Nodes[0]["status"]) != `"unknown"` {
		t.Fatalf("rotation served cached identity: %s", afterBytes)
	}
	for _, metric := range []string{"cpu_percent", "memory_percent", "load1", "uptime_seconds"} {
		if string(after.Nodes[0][metric]) != "null" {
			t.Fatalf("rotation retained %s: %s", metric, afterBytes)
		}
	}
}

func TestPublicPageCacheLivenessBoundaryLagsByAtMostTTL(t *testing.T) {
	f := newAgentHTTP(t, 2)
	_, token := publishCacheNode(t, f)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "boundary-cache-session", 25)), 1)
	f.now = f.now.Add(180 * time.Second)
	boundary, boundaryBytes := publicList(t, f, "/api/public/v1/nodes", nil)
	if len(boundary.Nodes) != 1 || string(boundary.Nodes[0]["status"]) != `"online"` {
		t.Fatalf("exact online boundary: %s", boundaryBytes)
	}
	// A rendered page can briefly retain its computed online status after the
	// durable 180-second boundary, including its original generated_at.
	f.now = f.now.Add(2*time.Second - time.Nanosecond)
	cached, cachedBytes := publicList(t, f, "/api/public/v1/nodes", nil)
	if !reflect.DeepEqual(cached, boundary) {
		t.Fatalf("page did not retain its computed status within TTL: %s", cachedBytes)
	}
	f.now = f.now.Add(time.Nanosecond)
	expired, expiredBytes := publicList(t, f, "/api/public/v1/nodes", nil)
	if string(expired.Nodes[0]["status"]) != `"offline"` || expired.GeneratedAt == boundary.GeneratedAt {
		t.Fatalf("online status persisted past TTL: %s", expiredBytes)
	}
}
