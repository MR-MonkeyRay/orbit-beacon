package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	vmTotal         = "orbitbeacon_history_total"
	vmSamples       = "orbitbeacon_history_samples"
	vmPeak          = "orbitbeacon_history_peak"
	vmResponseLimit = 16 << 20
)

type victoriaMetricsStore struct {
	base, token string
	client      *http.Client
	now         func() time.Time
}

var _ Store = (*victoriaMetricsStore)(nil)

// OpenVictoriaMetrics 保留本地历史文件供备份使用，启动时不访问远端。
func OpenVictoriaMetrics(ctx context.Context, dataDir, base, token string, nodes *sql.DB, opts Options) (*Service, error) {
	return openVictoriaMetrics(ctx, dataDir, nodes, opts, newVictoriaMetricsStore(base, token, nil, opts.Now))
}

func openVictoriaMetrics(ctx context.Context, dataDir string, nodes *sql.DB, opts Options, store *victoriaMetricsStore) (*Service, error) {
	db, err := OpenDatabase(ctx, filepath.Join(dataDir, "metrics.db"))
	if err != nil {
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return newService(store, nodes, opts), nil
}

func newVictoriaMetricsStore(base, token string, transport http.RoundTripper, now func() time.Time) *victoriaMetricsStore {
	if transport == nil {
		transport = &http.Transport{Proxy: nil, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	return &victoriaMetricsStore{base: strings.TrimRight(base, "/"), token: token, now: now, client: &http.Client{
		Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// request 不返回远端正文或底层错误文本，避免凭据及查询内容泄漏。
func (s *victoriaMetricsStore) request(ctx context.Context, operation, method, path, contentType string, body io.Reader) ([]byte, int, error) {
	fail := func(status int, reason string) error {
		return fmt.Errorf("victoriametrics %s (status %d): %s", operation, status, reason)
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, fmt.Errorf("%w: %w", fail(0, "context canceled"), err)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, body)
	if err != nil {
		return nil, 0, fail(0, "invalid request")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	response, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, fmt.Errorf("%w: %w", fail(0, "request failed"), ctx.Err())
		}
		return nil, 0, fail(0, "request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, vmResponseLimit+1))
	if err != nil {
		return nil, response.StatusCode, fail(response.StatusCode, "read failed")
	}
	if len(data) > vmResponseLimit {
		return nil, response.StatusCode, fail(response.StatusCode, "response too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.StatusCode, fail(response.StatusCode, "HTTP request failed")
	}
	return data, response.StatusCode, nil
}

func (s *victoriaMetricsStore) Write(ctx context.Context, buckets []Bucket) (int, error) {
	rows := 3 * len(buckets)
	if len(buckets) == 0 {
		return rows, nil
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, bucket := range buckets {
		for _, metric := range []struct {
			name  string
			value any
		}{{vmTotal, bucket.Total}, {vmSamples, bucket.Samples}, {vmPeak, bucket.Peak}} {
			line := struct {
				Metric     map[string]string `json:"metric"`
				Values     []any             `json:"values"`
				Timestamps []int64           `json:"timestamps"`
			}{map[string]string{"__name__": metric.name, "node": bucket.Node, "series": bucket.Series}, []any{metric.value}, []int64{(bucket.T + 30) * 1000}}
			if err := encoder.Encode(line); err != nil {
				return rows, errors.New("victoriametrics write (status 0): invalid sample")
			}
		}
	}
	_, _, err := s.request(ctx, "write", http.MethodPost, "/api/v1/import", "application/json", &body)
	return rows, err
}

func (s *victoriaMetricsStore) queryRange(ctx context.Context, query string, step int, start, end int64) (map[int64]float64, error) {
	form := url.Values{"query": {query}, "start": {strconv.FormatInt(start, 10)}, "end": {strconv.FormatInt(end, 10)}, "step": {strconv.Itoa(step) + "s"}, "nocache": {"1"}}
	data, status, err := s.request(ctx, "query", http.MethodPost, "/api/v1/query_range", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	var result struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &result) != nil || result.Status != "success" || len(result.Data.Result) > 1 {
		return nil, fmt.Errorf("victoriametrics query (status %d): invalid result", status)
	}
	points := make(map[int64]float64)
	for _, series := range result.Data.Result {
		for _, pair := range series.Values {
			var timestamp int64
			var value string
			if len(pair) != 2 || json.Unmarshal(pair[0], &timestamp) != nil || json.Unmarshal(pair[1], &value) != nil {
				return nil, fmt.Errorf("victoriametrics query (status %d): invalid point", status)
			}
			number, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return nil, fmt.Errorf("victoriametrics query (status %d): invalid value", status)
			}
			points[timestamp] = number
		}
	}
	return points, nil
}

func (s *victoriaMetricsStore) Query(ctx context.Context, node, series string, step int, start, end int64) ([]Point, error) {
	selector := "{node=" + strconv.Quote(node) + ",series=" + strconv.Quote(series) + "}"
	window := fmt.Sprintf("[%ds] offset -%ds", step, step)
	avg, err := s.queryRange(ctx, "sum_over_time("+vmTotal+selector+window+") / sum_over_time("+vmSamples+selector+window+")", step, start, end)
	if err != nil {
		return nil, err
	}
	peak, err := s.queryRange(ctx, "max_over_time("+vmPeak+selector+window+")", step, start, end)
	if err != nil {
		return nil, err
	}
	points := []Point{}
	for timestamp, value := range avg {
		if maximum, ok := peak[timestamp]; ok {
			points = append(points, Point{T: time.Unix(timestamp, 0).UTC(), Avg: value, Max: maximum})
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i].T.Before(points[j].T) })
	return points, nil
}

func (s *victoriaMetricsStore) labels(ctx context.Context, label, match string) ([]string, error) {
	form := url.Values{"match[]": {match}, "start": {strconv.FormatInt(s.now().Add(-2*Retention1h).Unix(), 10)}}
	data, status, err := s.request(ctx, label, http.MethodGet, "/api/v1/label/"+label+"/values?"+form.Encode(), "", nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if json.Unmarshal(data, &result) != nil || result.Status != "success" {
		return nil, fmt.Errorf("victoriametrics %s (status %d): invalid result", label, status)
	}
	return result.Data, nil
}

func (s *victoriaMetricsStore) Series(ctx context.Context, node string) ([]string, error) {
	return s.labels(ctx, "series", vmSamples+"{node="+strconv.Quote(node)+"}")
}
func (s *victoriaMetricsStore) NodeIDs(ctx context.Context, visit func(string)) error {
	ids, err := s.labels(ctx, "node", vmSamples)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		visit(id)
	}
	return nil
}
func (s *victoriaMetricsStore) DeleteNode(ctx context.Context, node string, yield func()) (int64, error) {
	form := url.Values{"match[]": {`{__name__=~"orbitbeacon_history_(total|samples|peak)",node=` + strconv.Quote(node) + `}`}}
	_, _, err := s.request(ctx, "delete", http.MethodPost, "/api/v1/admin/tsdb/delete_series", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	yield()
	return 0, err
}
func (s *victoriaMetricsStore) DeleteExpired(context.Context, time.Time, func()) (int64, error) {
	return 0, nil
}
func (s *victoriaMetricsStore) DatabaseStats() (sql.DBStats, sql.DBStats, bool) {
	return sql.DBStats{}, sql.DBStats{}, false
}
func (s *victoriaMetricsStore) Close() error { s.client.CloseIdleConnections(); return nil }
