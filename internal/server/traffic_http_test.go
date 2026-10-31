package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/traffic"
	"orbitbeacon/internal/timekey"
)

func TestTrafficAPIEnvelopeValidationAccessAndMethods(t *testing.T) {
	f := newHTTP(t, 2)
	second, _ := login(t, f, "admin-two")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "traffic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`UPDATE nodes SET billing_day=28, quota_bytes='1000' WHERE id=?`, node.ID); err != nil {
		t.Fatal(err)
	}
	// 测试夹具在真实时钟下运行，因此把行锚定到今天的周期；
	// 固定日期的周期计算由 traffic 包测试覆盖。
	now := time.Now().UTC()
	start, end := traffic.CycleBounds(now, 28)
	updated := now.Truncate(time.Hour)
	before, first, today := start.AddDate(0, 0, -1).Format(time.DateOnly), start.Format(time.DateOnly), updated.Format(time.DateOnly)
	for _, row := range []struct {
		day    string
		rx, tx int64
	}{{before, 10, 20}, {first, 30, 40}} {
		if _, err = f.db.Exec(`INSERT INTO traffic_daily(node_id,day,rx_bytes,tx_bytes) VALUES(?,?,?,?)`, node.ID, row.day, row.rx, row.tx); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		epoch  string
		rx, tx int64
	}{{"epoch-a", 5, 7}, {"epoch-b", 11, 13}} {
		if _, err = f.db.Exec(`INSERT INTO traffic_hourly(node_id,epoch,period_start,rx_bytes,tx_bytes,updated_at) VALUES(?,?,?,?,?,?)`, node.ID, row.epoch, timekey.Format(updated), row.rx, row.tx, timekey.Format(updated)); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string][2]string{before: {"10", "20"}, first: {"30", "40"}, today: {"16", "20"}}
	if first == today {
		want[today] = [2]string{"46", "60"}
	}

	route := "/api/v1/nodes/" + node.ID + "/traffic"
	response := request(t, f.handler, http.MethodGet, route+"?days=400", "", second, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	var envelope struct {
		BillingDay *int    `json:"billing_day"`
		QuotaBytes *string `json:"quota_bytes"`
		Cycle      struct {
			Start      time.Time `json:"start"`
			End        time.Time `json:"end"`
			RXBytes    string    `json:"rx_bytes"`
			TXBytes    string    `json:"tx_bytes"`
			TotalBytes string    `json:"total_bytes"`
		} `json:"cycle"`
		Daily []struct {
			Day     string `json:"day"`
			RXBytes string `json:"rx_bytes"`
			TXBytes string `json:"tx_bytes"`
		} `json:"daily"`
		UpdatedAt *time.Time `json:"updated_at"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.BillingDay == nil || *envelope.BillingDay != 28 || envelope.QuotaBytes == nil || *envelope.QuotaBytes != "1000" {
		t.Fatalf("configuration envelope=%s", response.Body)
	}
	if !envelope.Cycle.Start.Equal(start) || !envelope.Cycle.End.Equal(end) {
		t.Fatalf("cycle envelope=%s", response.Body)
	}
	if envelope.Cycle.RXBytes != "46" || envelope.Cycle.TXBytes != "60" || envelope.Cycle.TotalBytes != "106" {
		t.Fatalf("cycle totals=%s", response.Body)
	}
	if len(envelope.Daily) != len(want) {
		t.Fatalf("daily envelope=%s", response.Body)
	}
	for index, day := range envelope.Daily {
		if index > 0 && day.Day <= envelope.Daily[index-1].Day || want[day.Day] != [2]string{day.RXBytes, day.TXBytes} {
			t.Fatalf("daily envelope=%s", response.Body)
		}
	}
	if envelope.UpdatedAt == nil || !envelope.UpdatedAt.Equal(updated) {
		t.Fatalf("updated_at=%s", response.Body)
	}

	for _, query := range []string{"?days=0", "?days=401", "?days=nope", "?days=1&days=2", "?other=1", "?days=%zz"} {
		apiError(t, request(t, f.handler, http.MethodGet, route+query, "", second, "", ""), http.StatusBadRequest, "validation_failed")
	}
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/nodes/00000000-0000-4000-8000-000000000001/traffic", "", second, "", ""), http.StatusNotFound, "node_not_found")
	methodResponse := request(t, f.handler, http.MethodPost, route, "", second, "", "")
	apiError(t, methodResponse, http.StatusMethodNotAllowed, "method_not_allowed")
	if methodResponse.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("Allow=%q", methodResponse.Header().Get("Allow"))
	}
}
