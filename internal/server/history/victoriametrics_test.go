package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type vmRoundTripper func(*http.Request) (*http.Response, error)

func (f vmRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func vmResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func vmStore(t *testing.T, token string, fn vmRoundTripper) *victoriaMetricsStore {
	t.Helper()
	s := newVictoriaMetricsStore("http://vm.test/prefix/", token, fn, func() time.Time { return time.Unix(1800000000, 0) })
	t.Cleanup(func() { s.Close() })
	return s
}
func vmMatrix(values string) string {
	return `{"status":"success","data":{"result":[{"values":` + values + `}]}}`
}

func TestVictoriaMetricsWrite(t *testing.T) {
	for _, token := range []string{"", "test-secret"} {
		t.Run(token, func(t *testing.T) {
			calls := 0
			s := vmStore(t, token, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "POST" || r.URL.String() != "http://vm.test/prefix/api/v1/import" || r.Header.Get("Content-Type") != "application/json" {
					t.Fatalf("request=%v", r)
				}
				wantAuth := ""
				if token != "" {
					wantAuth = "Bearer " + token
				}
				if r.Header.Get("Authorization") != wantAuth {
					t.Fatal("authorization mismatch")
				}
				body, _ := io.ReadAll(r.Body)
				want := "{\"metric\":{\"__name__\":\"orbitbeacon_history_total\",\"node\":\"n1\",\"series\":\"cpu_percent\"},\"values\":[12.5],\"timestamps\":[1700000030000]}\n" +
					"{\"metric\":{\"__name__\":\"orbitbeacon_history_samples\",\"node\":\"n1\",\"series\":\"cpu_percent\"},\"values\":[2],\"timestamps\":[1700000030000]}\n" +
					"{\"metric\":{\"__name__\":\"orbitbeacon_history_peak\",\"node\":\"n1\",\"series\":\"cpu_percent\"},\"values\":[8],\"timestamps\":[1700000030000]}\n"
				if string(body) != want {
					t.Fatalf("body=%s want=%s", body, want)
				}
				return vmResponse(204, ""), nil
			})
			if rows, err := s.Write(context.Background(), nil); rows != 0 || err != nil || calls != 0 {
				t.Fatalf("empty rows=%d calls=%d err=%v", rows, calls, err)
			}
			if rows, err := s.Write(context.Background(), []Bucket{{Node: "n1", Series: "cpu_percent", T: 1700000000, Total: 12.5, Peak: 8, Samples: 2}}); rows != 3 || err != nil || calls != 1 {
				t.Fatalf("rows=%d calls=%d err=%v", rows, calls, err)
			}
		})
	}
}

func TestVictoriaMetricsQuery(t *testing.T) {
	for _, step := range []int{60, 300, 3600} {
		t.Run(strconv.Itoa(step), func(t *testing.T) {
			calls := 0
			node := `n"\节点`
			series := `disk_used_percent:/data/"a\ b/中文`
			selector := "{node=" + strconv.Quote(node) + ",series=" + strconv.Quote(series) + "}"
			window := fmt.Sprintf("[%ds] offset -%ds", step, step)
			queries := []string{"sum_over_time(" + vmTotal + selector + window + ") / sum_over_time(" + vmSamples + selector + window + ")", "max_over_time(" + vmPeak + selector + window + ")"}
			s := vmStore(t, "", func(r *http.Request) (*http.Response, error) {
				if r.Method != "POST" || r.URL.Path != "/prefix/api/v1/query_range" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
					t.Fatalf("request=%v", r)
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				want := url.Values{"query": {queries[calls]}, "start": {"60"}, "end": {"300"}, "step": {strconv.Itoa(step) + "s"}, "nocache": {"1"}}
				if !reflect.DeepEqual(r.PostForm, want) {
					t.Fatalf("form=%v want=%v", r.PostForm, want)
				}
				calls++
				if calls == 1 {
					return vmResponse(200, vmMatrix(`[[180,"NaN"],[60,"2.5"],[120,"4"],[240,"+Inf"]]`)), nil
				}
				return vmResponse(200, vmMatrix(`[[60,"7"],[180,"9"],[300,"10"],[240,"Inf"]]`)), nil
			})
			points, err := s.Query(context.Background(), node, series, step, 60, 300)
			if err != nil || calls != 2 || len(points) != 3 {
				t.Fatalf("points=%v calls=%d err=%v", points, calls, err)
			}
			if points[0].T.Unix() != 60 || points[0].Avg != 2.5 || points[0].Max != 7 || points[1].T.Unix() != 180 || !math.IsNaN(points[1].Avg) || !math.IsInf(points[2].Avg, 1) || !math.IsInf(points[2].Max, 1) {
				t.Fatalf("points=%+v", points)
			}
		})
	}
}

func TestVictoriaMetricsQueryResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		bad        bool
	}{
		{"empty", `{"status":"success","data":{"result":[]}}`, false},
		{"multiple", `{"status":"success","data":{"result":[{},{}]}}`, true},
		{"status", `{"status":"error","error":"private response"}`, true},
		{"json", `private response`, true},
		{"bad timestamp", vmMatrix(`[["oops","1"]]`), true},
		{"bad value", vmMatrix(`[[60,"private response"]]`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := vmStore(t, "secret-token", func(*http.Request) (*http.Response, error) { return vmResponse(200, tc.body), nil })
			points, err := s.Query(context.Background(), "node", "cpu_percent", 60, 0, 120)
			if (err != nil) != tc.bad {
				t.Fatalf("points=%v err=%v", points, err)
			}
			if !tc.bad && (points == nil || len(points) != 0) {
				t.Fatalf("empty points=%v", points)
			}
			if err != nil && (strings.Contains(err.Error(), "private response") || strings.Contains(err.Error(), "secret-token")) {
				t.Fatal("error leaked private text")
			}
		})
	}
}

type vmTrackedBody struct {
	io.Reader
	closed bool
	read   int
}

func (b *vmTrackedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *vmTrackedBody) Close() error { b.closed = true; return nil }
func TestVictoriaMetricsHTTPFailures(t *testing.T) {
	for _, code := range []int{199, 300, 302, 400, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			calls := 0
			body := &vmTrackedBody{Reader: strings.NewReader("response-secret")}
			s := vmStore(t, "token-secret", func(*http.Request) (*http.Response, error) {
				calls++
				r := vmResponse(code, "")
				r.Body = body
				r.Header.Set("Location", "http://other.test")
				return r, nil
			})
			rows, err := s.Write(context.Background(), []Bucket{{Node: "form-secret", Samples: 1}})
			if rows != 3 || err == nil || !strings.Contains(err.Error(), "write") || !strings.Contains(err.Error(), strconv.Itoa(code)) || calls != 1 || !body.closed || body.read != len("response-secret") {
				t.Fatalf("rows=%d err=%v calls=%d body=%+v", rows, err, calls, body)
			}
			for _, secret := range []string{"token-secret", "response-secret", "form-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("error leaked private text")
				}
			}
		})
	}
	t.Run("oversized", func(t *testing.T) {
		body := &vmTrackedBody{Reader: strings.NewReader(strings.Repeat("x", vmResponseLimit+20))}
		s := vmStore(t, "", func(*http.Request) (*http.Response, error) { r := vmResponse(200, ""); r.Body = body; return r, nil })
		_, err := s.Write(context.Background(), []Bucket{{Samples: 1}})
		if err == nil || !strings.Contains(err.Error(), "too large") || !body.closed || body.read != vmResponseLimit+1 {
			t.Fatalf("err=%v body=%+v", err, body)
		}
	})
	t.Run("transport", func(t *testing.T) {
		s := vmStore(t, "token-secret", func(*http.Request) (*http.Response, error) {
			return nil, errors.New("token-secret response-secret form-secret")
		})
		rows, err := s.Write(context.Background(), []Bucket{{Samples: 1}})
		if rows != 3 || err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("rows=%d err=%v", rows, err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		s := vmStore(t, "", func(*http.Request) (*http.Response, error) { t.Fatal("canceled request sent"); return nil, nil })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rows, err := s.Write(ctx, []Bucket{{Samples: 1}})
		if rows != 3 || !errors.Is(err, context.Canceled) {
			t.Fatalf("rows=%d err=%v", rows, err)
		}
	})
	t.Run("in flight cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := vmStore(t, "", func(r *http.Request) (*http.Response, error) {
			cancel()
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		if _, err := s.Write(ctx, []Bucket{{Samples: 1}}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestVictoriaMetricsLabelsAndMaintenance(t *testing.T) {
	node := `node"\ 中文`
	calls := 0
	s := vmStore(t, "", func(r *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1, 2:
			label, match := "series", vmSamples+"{node="+strconv.Quote(node)+"}"
			if calls == 2 {
				label, match = "node", vmSamples
			}
			want := url.Values{"match[]": {match}, "start": {strconv.FormatInt(time.Unix(1800000000, 0).Add(-2*Retention1h).Unix(), 10)}}
			if r.Method != "GET" || r.URL.Path != "/prefix/api/v1/label/"+label+"/values" || !reflect.DeepEqual(r.URL.Query(), want) {
				t.Fatalf("request=%v", r)
			}
			return vmResponse(200, `{"status":"success","data":["a","b"]}`), nil
		case 3:
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			want := url.Values{"match[]": {`{__name__=~"orbitbeacon_history_(total|samples|peak)",node=` + strconv.Quote(node) + `}`}}
			if r.Method != "POST" || r.URL.Path != "/prefix/api/v1/admin/tsdb/delete_series" || !reflect.DeepEqual(r.PostForm, want) {
				t.Fatalf("request=%v form=%v", r, r.PostForm)
			}
			return vmResponse(204, ""), nil
		default:
			t.Fatal("unexpected request")
			return nil, nil
		}
	})
	series, err := s.Series(context.Background(), node)
	if err != nil || !reflect.DeepEqual(series, []string{"a", "b"}) {
		t.Fatalf("series=%v err=%v", series, err)
	}
	var ids []string
	if err := s.NodeIDs(context.Background(), func(id string) { ids = append(ids, id) }); err != nil || !reflect.DeepEqual(ids, series) {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	yields := 0
	if removed, err := s.DeleteNode(context.Background(), node, func() { yields++ }); removed != 0 || err != nil || yields != 1 {
		t.Fatalf("removed=%d yields=%d err=%v", removed, yields, err)
	}
	if removed, err := s.DeleteExpired(context.Background(), time.Now(), func() { t.Fatal("unexpected yield") }); removed != 0 || err != nil || calls != 3 {
		t.Fatalf("removed=%d calls=%d err=%v", removed, calls, err)
	}
	if _, _, ok := s.DatabaseStats(); ok {
		t.Fatal("reported SQL pools")
	}
}

func TestVictoriaMetricsDeleteNodeReportsFailure(t *testing.T) {
	s := vmStore(t, "", func(*http.Request) (*http.Response, error) { return vmResponse(500, ""), nil })
	yielded := false
	if _, err := s.DeleteNode(context.Background(), "node", func() { yielded = true }); err == nil || !yielded {
		t.Fatalf("yielded=%v err=%v", yielded, err)
	}
}

func TestVictoriaMetricsConstruction(t *testing.T) {
	s := newVictoriaMetricsStore("http://vm.test", "", nil, func() time.Time { t.Fatal("now called at construction"); return time.Time{} })
	defer s.Close()
	tr, ok := s.client.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || s.client.Timeout != 10*time.Second {
		t.Fatalf("client=%+v", s.client)
	}
	if s.client.CheckRedirect(&http.Request{}, nil) != http.ErrUseLastResponse {
		t.Fatal("redirect allowed")
	}
}

func TestVictoriaMetricsServiceFailedWriteRetainsPending(t *testing.T) {
	calls := 0
	store := vmStore(t, "", func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("offline") })
	s := stoppedStoreService(t, store, nil, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	s.add(Sample{"node", now.Add(-time.Minute), map[string]float64{"cpu_percent": 20}})
	for i := 0; i < 2; i++ {
		if err := s.flush(context.Background(), now, false); err == nil || len(s.pending) != 1 {
			t.Fatalf("pending=%v err=%v", s.pending, err)
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	// 手工刷写后清空，避免测试清理再次触发预期失败。
	clear(s.pending)
}

func TestVictoriaMetricsOpenPreservesLocalDatabase(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			dir, err := os.MkdirTemp("", "orbitbeacon-f2-history-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			path := filepath.Join(dir, "metrics.db")
			if existing {
				db, err := OpenDatabase(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`INSERT INTO buckets VALUES('old','cpu_percent',60,60,10,10,1)`)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			store := vmStore(t, "", func(*http.Request) (*http.Response, error) { t.Fatal("startup made request"); return nil, nil })
			store.now = func() time.Time { t.Fatal("startup called clock"); return time.Time{} }
			s, err := openVictoriaMetrics(context.Background(), dir, nil, Options{}, store)
			if err != nil {
				t.Fatal(err)
			}
			if s.Database() != nil {
				t.Fatal("remote history exposed a local snapshot source")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if !existing {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("remote history created a local database: %v", err)
				}
				return
			}
			db, err := OpenDatabase(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM buckets`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if existing {
				want = 1
			}
			if count != want {
				t.Fatalf("count=%d want=%d", count, want)
			}
		})
	}
}

func TestVictoriaMetricsStoreContract(t *testing.T) {
	base := os.Getenv("ORBITBEACON_TEST_VM_URL")
	if base == "" {
		t.Skip("ORBITBEACON_TEST_VM_URL is not set")
	}
	testStoreContract(t, func(t *testing.T) Store {
		store := newVictoriaMetricsStore(base, "", nil, time.Now)
		t.Cleanup(func() { store.Close() })
		return store
	}, func(t *testing.T, store Store) {
		t.Helper()
		_, _, err := store.(*victoriaMetricsStore).request(context.Background(), "force flush", http.MethodGet, "/internal/force_flush", "", nil)
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestVictoriaMetricsWriteEscapedLabelsAndInvalidSample(t *testing.T) {
	calls := 0
	node, series := "node\"\\ 中文", "disk_used_percent:/\"\\ space/中文"
	s := vmStore(t, "", func(r *http.Request) (*http.Response, error) {
		calls++
		decoder := json.NewDecoder(r.Body)
		for i := 0; i < 6; i++ {
			var line struct {
				Metric     map[string]string `json:"metric"`
				Timestamps []int64           `json:"timestamps"`
			}
			if err := decoder.Decode(&line); err != nil {
				t.Fatal(err)
			}
			if line.Metric["node"] != node || line.Metric["series"] != series || !reflect.DeepEqual(line.Timestamps, []int64{(60 + 30) * 1000}) {
				t.Fatalf("line=%+v", line)
			}
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			t.Fatalf("extra=%v err=%v", extra, err)
		}
		return vmResponse(200, ""), nil
	})
	bucket := Bucket{Node: node, Series: series, T: 60, Total: 4, Peak: 4, Samples: 1}
	if rows, err := s.Write(context.Background(), []Bucket{bucket, bucket}); rows != 6 || err != nil {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
	bucket.Total = math.NaN()
	if rows, err := s.Write(context.Background(), []Bucket{bucket}); rows != 3 || err == nil || calls != 1 {
		t.Fatalf("rows=%d calls=%d err=%v", rows, calls, err)
	}
}

func TestVictoriaMetricsDecodeErrorsRetainHTTPStatus(t *testing.T) {
	s := vmStore(t, "", func(*http.Request) (*http.Response, error) {
		return vmResponse(202, `{"status":"error","error":"response-secret"}`), nil
	})
	_, err := s.Series(context.Background(), "node")
	if err == nil || !strings.Contains(err.Error(), "series (status 202)") || strings.Contains(err.Error(), "response-secret") {
		t.Fatalf("err=%v", err)
	}
	_, err = s.Query(context.Background(), "node", "cpu_percent", 60, 0, 60)
	if err == nil || !strings.Contains(err.Error(), "query (status 202)") || strings.Contains(err.Error(), "response-secret") {
		t.Fatalf("err=%v", err)
	}
}

type vmIdleTransport struct{ closed bool }

func (*vmIdleTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return vmResponse(204, ""), nil
}
func (tr *vmIdleTransport) CloseIdleConnections() { tr.closed = true }

func TestVictoriaMetricsCloseIdleConnections(t *testing.T) {
	tr := &vmIdleTransport{}
	store := newVictoriaMetricsStore("http://vm.test", "", tr, nil)
	if err := store.Close(); err != nil || !tr.closed {
		t.Fatalf("closed=%v err=%v", tr.closed, err)
	}
}
