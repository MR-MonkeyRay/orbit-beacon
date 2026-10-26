package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func TestConfigPatchRejectsNonCanonicalQuotaAtomically(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "canonical quota"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID + "/config"
	for _, quota := range []string{"01", "00", "00018446744073709551615", "+1", "-1", "1.0", " 1", "1 ", "18446744073709551616", ""} {
		t.Run(quota, func(t *testing.T) {
			before := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
			assertHTTPStatus(t, before, http.StatusOK)
			body, err := json.Marshal(map[string]any{"quota_bytes": quota, "report_interval_seconds": 30})
			if err != nil {
				t.Fatal(err)
			}
			response := request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL)
			object := apiError(t, response, http.StatusBadRequest, "validation_failed")
			var fields map[string]string
			if err := json.Unmarshal(object["fields"], &fields); err != nil {
				t.Fatal(err)
			}
			if fields["quota_bytes"] == "" {
				t.Error("quota rejection lacks field error")
			}
			after := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
			assertHTTPStatus(t, after, http.StatusOK)
			var beforeConfig, afterConfig v1.DesiredConfig
			if err := json.Unmarshal(before.Body.Bytes(), &beforeConfig); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(after.Body.Bytes(), &afterConfig); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeConfig, afterConfig) {
				t.Errorf("rejected quota partially changed desired configuration/revision: before=%+v after=%+v", beforeConfig, afterConfig)
			}
		})
	}
}

func TestEveryPersistedCanonicalQuotaIsAcceptedByAgent(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "quota round trip"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID + "/config"
	initial := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
	assertHTTPStatus(t, initial, http.StatusOK)
	var config v1.DesiredConfig
	if err := json.Unmarshal(initial.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(t.TempDir(), "desired.json")
	store, err := service.OpenConfigStore(storePath, config)
	if err != nil {
		t.Fatal(err)
	}
	for _, quota := range []string{"0", "1", "9", "10", "9007199254740993", "9223372036854775808", "18446744073709551615"} {
		t.Run(quota, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"quota_bytes": quota})
			if err != nil {
				t.Fatal(err)
			}
			response := request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL)
			assertHTTPStatus(t, response, http.StatusOK)
			persisted := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
			assertHTTPStatus(t, persisted, http.StatusOK)
			var desired v1.DesiredConfig
			if err := json.Unmarshal(persisted.Body.Bytes(), &desired); err != nil {
				t.Fatal(err)
			}
			if desired.QuotaBytes != quota {
				t.Fatalf("persisted quota lost exact decimal representation: got %q want %q", desired.QuotaBytes, quota)
			}
			if err := store.Apply(desired); err != nil {
				t.Fatalf("server-persisted quota cannot be applied by agent: %v", err)
			}
			reopened, err := service.OpenConfigStore(storePath, config)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reopened.Current(), desired) {
				t.Error("agent quota/config round trip changed persisted server value")
			}
		})
	}
}

func TestConfigPatchInterfaceConstraintsAreAtomicAndAgentCompatible(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "interface contract"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID + "/config"
	for _, tt := range []struct {
		name       string
		interfaces []string
	}{
		{"duplicate", []string{"eth0", "eth0"}},
		{"nine entries", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		{"empty", []string{""}},
		{"blank", []string{" \t "}},
		{"NUL", []string{"eth\x000"}},
		{"CR", []string{"eth0\rheader"}},
		{"LF", []string{"eth0\nheader"}},
		{"256 bytes", []string{strings.Repeat("a", 256)}},
		{"multibyte over 255 bytes", []string{strings.Repeat("é", 128)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
			assertHTTPStatus(t, before, http.StatusOK)
			body, err := json.Marshal(map[string]any{"interfaces": tt.interfaces, "quota_bytes": "42", "report_interval_seconds": 30})
			if err != nil {
				t.Fatal(err)
			}
			response := request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL)
			object := apiError(t, response, http.StatusBadRequest, "validation_failed")
			var fields map[string]string
			if err := json.Unmarshal(object["fields"], &fields); err != nil {
				t.Fatal(err)
			}
			if fields["interfaces"] == "" {
				t.Error("interface rejection lacks field error")
			}
			after := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
			assertHTTPStatus(t, after, http.StatusOK)
			if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
				t.Error("invalid interfaces changed persisted config or revision")
			}
		})
	}
	for _, interfaces := range [][]string{{}, {"a"}, {strings.Repeat("a", 255)}, {strings.Repeat("é", 127) + "a"}, {"a", "b", "c", "d", "e", "f", "g", "h"}} {
		body, err := json.Marshal(map[string]any{"interfaces": interfaces})
		if err != nil {
			t.Fatal(err)
		}
		response := request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL)
		assertHTTPStatus(t, response, http.StatusOK)
		var desired v1.DesiredConfig
		if err := json.Unmarshal(response.Body.Bytes(), &desired); err != nil {
			t.Fatal(err)
		}
		if err := desired.Validate(); err != nil {
			t.Errorf("valid persisted interface config rejected by agent protocol: %v", err)
		}
		if !reflect.DeepEqual(desired.Interfaces, interfaces) {
			t.Errorf("server changed valid interface selection: got=%v want=%v", desired.Interfaces, interfaces)
		}
	}
}

func TestConfigPatchRejectsAggregateReportSizeBeforeRevisionChange(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "report budget"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID + "/config"
	patch := func(value any, status int) {
		t.Helper()
		before := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		response := request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL)
		assertHTTPStatus(t, response, status)
		if status == http.StatusBadRequest {
			after := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
			if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
				t.Fatal("oversized selection changed configuration or revision")
			}
		}
	}
	paths := make([]string, 4)
	for i := range paths {
		paths[i] = "/" + string(rune('a'+i)) + strings.Repeat("<", 1000)
	}
	patch(map[string]any{"disk_paths": paths}, http.StatusBadRequest)
	paths = []string{"/" + strings.Repeat("<", 800), "/" + strings.Repeat("&", 800)}
	patch(map[string]any{"disk_paths": paths}, http.StatusOK)
	interfaces := make([]string, 8)
	for i := range interfaces {
		interfaces[i] = strings.Repeat("<", 254) + string(rune('a'+i))
	}
	patch(map[string]any{"interfaces": interfaces}, http.StatusBadRequest)
	patch(map[string]any{"disk_paths": []string{"/"}, "interfaces": []string{"eth0"}}, http.StatusOK)
}
