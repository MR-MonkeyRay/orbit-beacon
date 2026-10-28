package backup

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/egress"
)

type objectRoundTripFunc func(*http.Request) (*http.Response, error)

func (f objectRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func objectResponse(r *http.Request, status int, body string) *http.Response {
	response := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
	response.Header.Set("Content-Type", "application/xml")
	response.Header.Set("ETag", `"part-etag"`)
	return response
}

func objectFile(t *testing.T, size int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "archive.tar.gz.age")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func objectTestUploader(rt http.RoundTripper) *ObjectUploader {
	u := NewObjectUploader()
	u.client = func(ObjectDestination) *http.Client {
		return &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return u
}

func objectDestination() ObjectDestination {
	return ObjectDestination{Provider: "s3_compatible", Endpoint: "https://storage.example.com", Region: "test-region", Bucket: "backup-bucket", PathStyle: true}
}

var objectTestCredentials = ObjectCredentials{AccessKeyID: "TEST_ACCESS_KEY", SecretAccessKey: "TEST_SECRET_KEY", SessionToken: "TEST_SESSION_TOKEN"}

func checkObjectRequest(t *testing.T, r *http.Request, length int64, region string) {
	t.Helper()
	if r.ContentLength != length {
		t.Errorf("ContentLength=%d want %d", r.ContentLength, length)
	}
	if len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 || r.Header.Get("X-Amz-Trailer") != "" || strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		t.Errorf("streaming checksum/trailer request: headers=%v", r.Header)
	}
	if !strings.Contains(r.Header.Get("Authorization"), "/"+region+"/s3/aws4_request") {
		t.Errorf("wrong signing scope: %s", r.Header.Get("Authorization"))
	}
	if r.Header.Get("X-Amz-Security-Token") != objectTestCredentials.SessionToken {
		t.Error("static session token missing")
	}
	if remaining, ok := r.Context().Deadline(); !ok || time.Until(remaining) > 5*time.Minute {
		t.Error("missing bounded request deadline")
	}
	for _, h := range []string{"X-Amz-Acl", "X-Amz-Server-Side-Encryption", "X-Amz-Tagging"} {
		if r.Header.Get(h) != "" {
			t.Errorf("unexpected vendor feature %s", h)
		}
	}
}

func TestObjectUploaderSignedProfiles(t *testing.T) {
	profiles := []struct {
		name, provider, endpoint, region, host, path string
		pathStyle                                    bool
	}{
		{"cos", "cos", "https://cos.ap-shanghai.myqcloud.com", "ap-shanghai", "backup-bucket.cos.ap-shanghai.myqcloud.com", "/prefix/a%20b%2B.age", false},
		{"oss", "oss", "https://s3.oss-cn-hangzhou.aliyuncs.com", "cn-hangzhou", "backup-bucket.s3.oss-cn-hangzhou.aliyuncs.com", "/prefix/a%20b%2B.age", false},
		{"oss-internal", "oss", "https://s3.oss-cn-hangzhou-internal.aliyuncs.com", "cn-hangzhou", "backup-bucket.s3.oss-cn-hangzhou-internal.aliyuncs.com", "/prefix/a%20b%2B.age", false},
		{"oss-cname", "oss", "https://archives.example.com:9443", "cn-hangzhou", "archives.example.com:9443", "/prefix/a%20b%2B.age", false},
		{"r2", "r2", "https://account.r2.cloudflarestorage.com", "auto", "account.r2.cloudflarestorage.com", "/backup-bucket/prefix/a%20b%2B.age", true},
		{"aws", "s3", "https://s3.eu-west-1.amazonaws.com", "eu-west-1", "backup-bucket.s3.eu-west-1.amazonaws.com", "/prefix/a%20b%2B.age", false},
		{"compatible-path", "s3_compatible", "https://storage.example.com", "custom", "storage.example.com", "/backup-bucket/prefix/a%20b%2B.age", true},
		{"compatible-virtual", "s3_compatible", "https://storage.example.com", "custom", "backup-bucket.storage.example.com", "/prefix/a%20b%2B.age", false},
	}
	p := objectFile(t, 1234)
	for _, tc := range profiles {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			u := objectTestUploader(objectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				checkObjectRequest(t, r, 1234, tc.region)
				if r.Method != http.MethodPut || r.URL.Host != tc.host || r.URL.EscapedPath() != tc.path {
					t.Errorf("target=%s %s", r.Method, r.URL)
				}
				if r.Header.Get("X-Amz-Meta-Sha256") != "ciphertext-hash" {
					t.Error("archive hash metadata missing")
				}
				n, err := io.Copy(io.Discard, r.Body)
				if err != nil || n != 1234 {
					t.Errorf("body=%d %v", n, err)
				}
				return objectResponse(r, 200, ""), nil
			}))
			d := ObjectDestination{Provider: tc.provider, Endpoint: tc.endpoint, Region: tc.region, Bucket: "backup-bucket", PathStyle: tc.pathStyle}
			result, err := u.Upload(context.Background(), d, objectTestCredentials, p, "prefix/a b+.age", "ciphertext-hash")
			if err != nil || result.OutcomeUnknown || calls != 1 {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
			}
		})
	}
}

func TestObjectUploaderMultipartSerialFile(t *testing.T) {
	const size = 32*1024*1024 + 17
	p := objectFile(t, size)
	var operations []string
	var total int64
	u := objectTestUploader(objectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		switch {
		case r.Method == "POST" && q.Has("uploads"):
			operations = append(operations, "create")
			if r.Header.Get("X-Amz-Meta-Sha256") != "hash" {
				t.Error("multipart hash missing")
			}
			return objectResponse(r, 200, `<InitiateMultipartUploadResult><UploadId>upload-one</UploadId></InitiateMultipartUploadResult>`), nil
		case r.Method == "PUT" && q.Get("uploadId") == "upload-one":
			operations = append(operations, "part"+q.Get("partNumber"))
			want := int64(16 * 1024 * 1024)
			if q.Get("partNumber") == "3" {
				want = 17
			}
			checkObjectRequest(t, r, want, "test-region")
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil || n != want {
				t.Errorf("part body %d %v", n, err)
			}
			total += n
			return objectResponse(r, 200, ""), nil
		case r.Method == "POST" && q.Get("uploadId") == "upload-one":
			operations = append(operations, "complete")
			body, err := io.ReadAll(r.Body)
			if err != nil || strings.Count(string(body), "<Part>") != 3 {
				t.Errorf("completion=%s %v", body, err)
			}
			return objectResponse(r, 200, `<CompleteMultipartUploadResult><ETag>done</ETag></CompleteMultipartUploadResult>`), nil
		default:
			t.Fatalf("unexpected operation %s %s (List/Delete forbidden)", r.Method, r.URL)
			return nil, errors.New("unexpected operation")
		}
	}))
	result, err := u.Upload(context.Background(), objectDestination(), objectTestCredentials, p, "archive.age", "hash")
	if err != nil || result.OutcomeUnknown || total != size || strings.Join(operations, ",") != "create,part1,part2,part3,complete" {
		t.Fatalf("result=%+v err=%v total=%d operations=%v", result, err, total, operations)
	}
}

func TestObjectUploaderMultipartAbortIndependentContext(t *testing.T) {
	for _, failure := range []string{"part", "complete"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			aborts := 0
			u := objectTestUploader(objectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				q := r.URL.Query()
				if q.Has("uploads") {
					return objectResponse(r, 200, `<InitiateMultipartUploadResult><UploadId>id</UploadId></InitiateMultipartUploadResult>`), nil
				}
				if r.Method == "DELETE" {
					aborts++
					if r.Context().Err() != nil {
						t.Error("abort inherited canceled task")
					}
					if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 10*time.Second {
						t.Error("abort deadline missing")
					}
					return objectResponse(r, 204, ""), nil
				}
				if r.Method == "PUT" && failure == "complete" {
					return objectResponse(r, 200, ""), nil
				}
				cancel()
				if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteHeaders != nil {
					trace.WroteHeaders()
				}
				return nil, io.ErrUnexpectedEOF
			}))
			result, err := u.Upload(ctx, objectDestination(), objectTestCredentials, objectFile(t, 16*1024*1024+1), "archive.age", "hash")
			if err == nil || aborts != 1 || result.OutcomeUnknown != (failure == "complete") {
				t.Fatalf("result=%+v err=%v aborts=%d", result, err, aborts)
			}
		})
	}
}

func TestObjectUploaderOutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dispatched bool
		status     int
		unknown    bool
		code       string
	}{
		{"before-dispatch", false, 0, false, "storage_unreachable"},
		{"lost-response", true, 0, true, "storage_upload_failed"},
		{"access-denied", true, 403, false, "storage_auth_failed"},
		{"server-uncertain", true, 503, true, "storage_upload_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			u := objectTestUploader(objectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if tc.dispatched {
					if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteHeaders != nil {
						trace.WroteHeaders()
					}
				}
				if tc.status != 0 {
					return objectResponse(r, tc.status, `<Error><Code>AccessDenied</Code><Message>SECRET https://credentials.example.com</Message></Error>`), nil
				}
				return nil, fmt.Errorf("SECRET https://credentials.example.com: %w", io.ErrUnexpectedEOF)
			}))
			result, err := u.Upload(context.Background(), objectDestination(), objectTestCredentials, objectFile(t, 1), "archive.age", "hash")
			var safe *Error
			if !errors.As(err, &safe) || safe.Code != tc.code || safe.Err != nil || result.OutcomeUnknown != tc.unknown || calls != 1 {
				t.Fatalf("result=%+v error=%v calls=%d", result, err, calls)
			}
		})
	}
}

type objectResolver []net.IPAddr

func (r objectResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) { return r, nil }

func TestObjectTransportEgressAndTLS(t *testing.T) {
	for _, ips := range [][]string{{"127.0.0.1"}, {"169.254.169.254"}, {"169.254.1.1"}, {"::1"}, {"fe80::1"}, {"8.8.8.8", "127.0.0.1"}} {
		var resolver objectResolver
		for _, ip := range ips {
			resolver = append(resolver, net.IPAddr{IP: net.ParseIP(ip)})
		}
		d := objectDestination()
		d.AllowPrivateNetworks = true
		dials := 0
		tr := newObjectTransport(d, resolver, func(context.Context, string, string) (net.Conn, error) {
			dials++
			return nil, errors.New("should not dial")
		})
		_, err := tr.DialContext(context.Background(), "tcp", "storage.example.com:443")
		if !errors.Is(err, egress.ErrDenied) || dials != 0 {
			t.Errorf("ips=%v error=%v dials=%d", ips, err, dials)
		}
		if tr.Proxy != nil || tr.TLSClientConfig == nil || tr.TLSClientConfig.InsecureSkipVerify || tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
			t.Error("unsafe production transport")
		}
		tr.CloseIdleConnections()
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS accepted") }))
	defer server.Close()
	d := objectDestination()
	tr := newObjectTransport(d, objectResolver{{IP: net.ParseIP("8.8.8.8")}}, func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	})
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	if response, err := client.Get("https://storage.example.com"); err == nil {
		response.Body.Close()
		t.Fatal("untrusted TLS certificate accepted")
	}
}

func TestObjectMultipartPartSize(t *testing.T) {
	for _, size := range []int64{16*1024*1024 + 1, 32*1024*1024 + 17, 32 * 1024 * 1024 * 10000, 32*1024*1024*10000 + 1} {
		part := objectPartSize(size)
		if part < 16*1024*1024 || part%(1024*1024) != 0 || (size+part-1)/part > 10000 {
			t.Errorf("size=%d part=%d", size, part)
		}
	}
}

type objectResolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f objectResolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

func TestObjectUploaderGuardsFinalVirtualHost(t *testing.T) {
	d := objectDestination()
	d.PathStyle = false
	d.AllowPrivateNetworks = true
	lookups, dials := 0, 0
	tr := newObjectTransport(d, objectResolverFunc(func(ctx context.Context, host string) ([]net.IPAddr, error) {
		lookups++
		if host != "backup-bucket.storage.example.com" {
			t.Errorf("checked wrong hostname %s", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	}), func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("must not dial")
	})
	defer tr.CloseIdleConnections()
	u := objectTestUploader(tr)
	result, err := u.Upload(context.Background(), d, objectTestCredentials, objectFile(t, 1), "archive.age", "hash")
	var safe *Error
	if !errors.As(err, &safe) || safe.Code != "storage_unreachable" || result.OutcomeUnknown || lookups != 1 || dials != 0 {
		t.Fatalf("result=%+v error=%v lookups=%d dials=%d", result, err, lookups, dials)
	}
}

func TestObjectTransportPrivateOptInPinsLiteral(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		d := objectDestination()
		d.AllowPrivateNetworks = allowed
		dials := 0
		tr := newObjectTransport(d, objectResolver{{IP: net.ParseIP("10.11.12.13")}}, func(ctx context.Context, network, address string) (net.Conn, error) {
			dials++
			if address != "10.11.12.13:443" {
				t.Errorf("not pinned to checked literal: %s", address)
			}
			return nil, io.EOF
		})
		_, err := tr.DialContext(context.Background(), "tcp", "storage.example.com:443")
		if allowed && (dials != 1 || !errors.Is(err, io.EOF)) || !allowed && (dials != 0 || !errors.Is(err, egress.ErrDenied)) {
			t.Errorf("allowed=%v dials=%d error=%v", allowed, dials, err)
		}
		tr.CloseIdleConnections()
	}
}

func TestObjectUploaderCreateFailureDoesNotClaimUnknown(t *testing.T) {
	calls := 0
	u := objectTestUploader(objectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || !r.URL.Query().Has("uploads") {
			t.Fatalf("unexpected operation %s %s", r.Method, r.URL)
		}
		if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteHeaders != nil {
			trace.WroteHeaders()
		}
		return nil, io.ErrUnexpectedEOF
	}))
	result, err := u.Upload(context.Background(), objectDestination(), objectTestCredentials, objectFile(t, 16*1024*1024+1), "archive.age", "hash")
	if err == nil || result.OutcomeUnknown || calls != 1 {
		t.Fatalf("result=%+v error=%v calls=%d", result, err, calls)
	}
}

func TestObjectUploaderDoesNotFollowRedirects(t *testing.T) {
	calls := 0
	u := objectTestUploader(objectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		response := objectResponse(r, http.StatusTemporaryRedirect, "")
		response.Header.Set("Location", "https://127.0.0.1/metadata")
		return response, nil
	}))
	result, err := u.Upload(context.Background(), objectDestination(), objectTestCredentials, objectFile(t, 1), "archive.age", "hash")
	if err == nil || result.OutcomeUnknown || calls != 1 {
		t.Fatalf("result=%+v error=%v calls=%d", result, err, calls)
	}
}

func TestObjectUploaderCompleteResponseClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		unknown bool
		code    string
	}{
		{"rejected", 403, `<Error><Code>AccessDenied</Code></Error>`, false, "storage_auth_failed"},
		{"embedded-rejection", 200, `<Error><Code>InvalidPart</Code><Message>not committed</Message></Error>`, false, "storage_upload_failed"},
		{"truncated", 200, `<CompleteMultipartUploadResult><ETag>`, true, "storage_upload_failed"},
		{"server-uncertain", 503, `<Error><Code>InternalError</Code></Error>`, true, "storage_upload_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			aborts, completes := 0, 0
			u := objectTestUploader(objectRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteHeaders != nil {
					trace.WroteHeaders()
				}
				if r.URL.Query().Has("uploads") {
					return objectResponse(r, 200, `<InitiateMultipartUploadResult><UploadId>id</UploadId></InitiateMultipartUploadResult>`), nil
				}
				if r.Method == "PUT" {
					return objectResponse(r, 200, ""), nil
				}
				if r.Method == "DELETE" {
					aborts++
					return objectResponse(r, 204, ""), nil
				}
				completes++
				return objectResponse(r, tc.status, tc.body), nil
			}))
			result, err := u.Upload(context.Background(), objectDestination(), objectTestCredentials, objectFile(t, 16*1024*1024+1), "archive.age", "hash")
			var safe *Error
			if !errors.As(err, &safe) || safe.Code != tc.code || safe.Err != nil || result.OutcomeUnknown != tc.unknown || aborts != 1 || completes != 1 {
				t.Fatalf("result=%+v error=%v aborts=%d completes=%d", result, err, aborts, completes)
			}
		})
	}
}
