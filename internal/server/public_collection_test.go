package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func TestPublicCollectionExcludesInactiveNodesAndRawSeries(t *testing.T) {
	f := newAgentHTTP(t, 8)
	ctx := context.Background()
	var visible, token string
	enabled, name := true, "Explicit public"
	for i, kind := range []string{"visible", "private", "disabled", "deleted"} {
		id, credential, _ := enrollHTTP(t, f, "private-"+kind, byte(100+i))
		if kind == "private" {
			continue
		}
		if _, err := f.nodes.PatchPublicSettings(ctx, id, nodes.PublicSettingsPatch{Enabled: &enabled, Name: &name}); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "visible":
			visible, token = id, credential
		case "disabled":
			if err := f.nodes.SetDisabled(ctx, id, true); err != nil {
				t.Fatal(err)
			}
		case "deleted":
			if err := f.nodes.Delete(ctx, id, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	pending, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "private-pending"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.nodes.PatchPublicSettings(ctx, pending.ID, nodes.PublicSettingsPatch{Enabled: &enabled, Name: &name}); err != nil {
		t.Fatal(err)
	}
	var report v1.Report
	if err := json.Unmarshal([]byte(reportBody(1, 1, "private-report-session", 20)), &report); err != nil {
		t.Fatal(err)
	}
	total, used := uint64(100), uint64(50)
	rate := 1.0
	window := int64(10000)
	path, iface := "/private-disk-203.0.113.99", "secret0"
	report.Metrics.Disks = []v1.DiskMetrics{{Path: path, TotalBytes: &total, UsedBytes: &used, SampledAt: f.now}}
	report.Metrics.Networks = []v1.NetworkMetrics{{Name: iface, RxBytesPerSec: &rate, TxBytesPerSec: &rate}}
	for _, key := range []string{v1.DiskMetricKey(path), v1.NetworkMetricKey(iface)} {
		report.Metrics.MetricStatus[key] = "ok"
		report.Metrics.Sources[key] = v1.SourceMetadata{SampleID: "private-source-2001:db8::99", SourceIntervalMS: 10000, MeasurementWindowMS: &window}
	}
	if _, err := f.ingest.AcceptToken(ctx, token, report); err != nil {
		t.Fatal(err)
	}
	result, raw := publicList(t, f, "/api/public/v1/nodes", nil)
	if len(result.Nodes) != 1 || string(result.Nodes[0]["name"]) != `"Explicit public"` {
		t.Fatalf("inactive nodes exposed: %s", raw)
	}
	for _, secret := range []string{path, iface, "private-source-2001:db8::99", "private-report-session", visible, token} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("private raw series/identity leaked %q", secret)
		}
	}
	ids, _, err := f.nodes.ListPublicIDs(ctx, 50, 0)
	if err != nil || len(ids) != 1 {
		t.Fatalf("candidates: %v %v", ids, err)
	}
	enabled = false
	if _, err := f.nodes.PatchPublicSettings(ctx, visible, nodes.PublicSettingsPatch{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ingest.PublicNode(ctx, ids[0], f.now); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("withdrawn candidate retained projection: %v", err)
	}
}
