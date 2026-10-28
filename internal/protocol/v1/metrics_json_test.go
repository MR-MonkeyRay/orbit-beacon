package v1_test

import (
	"encoding/json"
	"reflect"
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestTelemetryExtensionsAreDiscardedWithoutChangingKnownMetrics(t *testing.T) {
	original := completeMetrics(t)
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, extension := range []string{`null`, `42`, `"future"`, `false`, `[]`, `{"nested":[1,true]}`} {
		t.Run(extension, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			fields["future_metric"] = json.RawMessage(extension)
			for _, name := range []string{"metric_status", "sources"} {
				var metadata map[string]json.RawMessage
				if err := json.Unmarshal(fields[name], &metadata); err != nil {
					t.Fatal(err)
				}
				metadata["future_metric"] = json.RawMessage(extension)
				fields[name], err = json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
			}
			body, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			var decoded v1.Metrics
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			if err := decoded.Validate(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, original) {
				t.Fatalf("extension changed known metrics or survived decoding: got %#v want %#v", decoded, original)
			}
		})
	}
}

func TestTelemetryRejectsOrphanKnownSeriesMetadata(t *testing.T) {
	for _, name := range []string{"metric_status", "sources"} {
		for _, key := range []string{"disk:/not-reported", "network:ghost"} {
			t.Run(name+"/"+key, func(t *testing.T) {
				body, err := json.Marshal(completeMetrics(t))
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(body, &fields)
				var metadata map[string]json.RawMessage
				_ = json.Unmarshal(fields[name], &metadata)
				metadata[key] = json.RawMessage(`null`)
				fields[name], _ = json.Marshal(metadata)
				body, _ = json.Marshal(fields)
				var decoded v1.Metrics
				if err := json.Unmarshal(body, &decoded); err == nil && decoded.Validate() == nil {
					t.Fatal("orphan metadata was accepted")
				}
			})
		}
	}
}
