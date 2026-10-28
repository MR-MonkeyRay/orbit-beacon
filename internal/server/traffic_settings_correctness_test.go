package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"orbitbeacon/internal/server/nodes"
)

func TestTrafficSettingsRejectsNonCanonicalQuotaAtomically(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "canonical quota"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID + "/traffic-settings"
	for _, quota := range []string{"01", "00", "00018446744073709551615", "+1", "-1", "1.0", " 1", "1 ", "18446744073709551616", ""} {
		t.Run(quota, func(t *testing.T) {
			before := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
			assertHTTPStatus(t, before, http.StatusOK)
			body, err := json.Marshal(map[string]any{"quota_bytes": quota, "billing_day": 2})
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
			var beforeConfig, afterConfig nodes.TrafficSettings
			if err := json.Unmarshal(before.Body.Bytes(), &beforeConfig); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(after.Body.Bytes(), &afterConfig); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeConfig, afterConfig) {
				t.Errorf("rejected quota partially changed traffic settings: before=%+v after=%+v", beforeConfig, afterConfig)
			}
		})
	}
}

func TestTrafficSettingsPreservesEveryCanonicalQuotaExactly(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "quota round trip"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID + "/traffic-settings"
	initial := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
	assertHTTPStatus(t, initial, http.StatusOK)
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
			var desired nodes.TrafficSettings
			if err := json.Unmarshal(persisted.Body.Bytes(), &desired); err != nil {
				t.Fatal(err)
			}
			if desired.QuotaBytes != quota {
				t.Fatalf("persisted quota lost exact decimal representation: got %q want %q", desired.QuotaBytes, quota)
			}
		})
	}
}
