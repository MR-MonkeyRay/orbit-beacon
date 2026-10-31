package backup

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyendpoints "github.com/aws/smithy-go/endpoints"

	"orbitbeacon/internal/server/egress"
)

const objectMinimumPartSize int64 = 16 * 1024 * 1024

// ObjectUploader 使用静态凭据和 S3 协议发布单个归档。
// 涉及安全的传输替换入口不导出。实例可共享，每次上传使用独立的客户端和连接。
type ObjectUploader struct {
	client func(ObjectDestination) *http.Client
}

func NewObjectUploader() *ObjectUploader { return &ObjectUploader{} }

func newObjectTransport(d ObjectDestination, resolver egress.Resolver, dial func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	guard := &egress.Dialer{Resolver: resolver, Dial: dial, AllowPrivate: d.AllowPrivateNetworks, DenyLocal: true}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           guard.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          1,
		MaxIdleConnsPerHost:   1,
	}
}

// OSS 解析器只在签名之前更改解析后的 URI。保持默认解析器的属性，
// 从而保留 SDK 的签名区域/服务。
type objectOSSResolver struct {
	base     s3.EndpointResolverV2
	endpoint url.URL
}

func (r objectOSSResolver) ResolveEndpoint(ctx context.Context, p s3.EndpointParameters) (smithyendpoints.Endpoint, error) {
	resolved, err := r.base.ResolveEndpoint(ctx, p)
	if err != nil {
		return resolved, err
	}
	resolved.URI = r.endpoint
	return resolved, nil
}

func objectOfficialOSS(host string) bool {
	return strings.HasPrefix(host, "s3.oss-") && strings.HasSuffix(host, ".aliyuncs.com")
}

// 每个操作都有独立的派发记录。Create/UploadPart 不会让后续的
// Complete 看起来像已派发，Abort 也不会改变其结果。
type objectRequestState struct {
	dispatched atomic.Bool
	status     atomic.Int32
}
type objectRequestKey struct{}

type objectHTTPClient struct{ client *http.Client }

type objectResponseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *objectResponseBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }

func (c objectHTTPClient) Do(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	state, _ := ctx.Value(objectRequestKey{}).(*objectRequestState)
	if state != nil {
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			WroteHeaders: func() { state.dispatched.Store(true) },
		})
	}
	response, err := c.client.Do(r.WithContext(ctx))
	if response != nil && state != nil {
		state.status.Store(int32(response.StatusCode))
	}
	if err != nil {
		cancel()
		return response, err
	}
	response.Body = &objectResponseBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

func objectRequest(ctx context.Context) (context.Context, *objectRequestState) {
	state := &objectRequestState{}
	return context.WithValue(ctx, objectRequestKey{}, state), state
}

func objectUploadError(state *objectRequestState, err error, final bool) (UploadResult, error) {
	if err == nil {
		return UploadResult{}, nil
	}
	status := state.status.Load()
	// 4xx（或重定向）明确拒绝了本次请求。最终写入失败但无响应、
	// 成功响应畸形或服务端 5xx，均无法证明对象未提交。
	// 绝不重试该外部写入。
	unknown := final && state.dispatched.Load() && (status == 0 || status >= 500 || status >= 200 && status < 300)
	// CompleteMultipartUpload 可能在 HTTP 200 内部返回显式的 XML 错误。
	// SDK 将其解析为 APIError；与截断的成功响应体不同，
	// 这是确定性的拒绝，而非未确认的传输结果。
	var apiError smithy.APIError
	if status >= 200 && status < 300 && errors.As(err, &apiError) {
		unknown = false
	}
	code := "storage_upload_failed"
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		code = "storage_auth_failed"
	} else if status == 0 && !state.dispatched.Load() {
		code = "storage_unreachable"
	}
	// 不保留 SDK 错误：其解包链可能包含带凭据的 URL、
	// 服务端可控的消息或响应体。
	return UploadResult{OutcomeUnknown: unknown}, &Error{Code: code}
}

func objectPartSize(size int64) int64 {
	const mib int64 = 1024 * 1024
	part := (size + 9999) / 10000
	part = (part + mib - 1) / mib * mib
	if part < objectMinimumPartSize {
		return objectMinimumPartSize
	}
	return part
}

func (u *ObjectUploader) Upload(ctx context.Context, destination ObjectDestination, input ObjectCredentials, path, key, sha256 string) (UploadResult, error) {
	if destination.Validate() != nil || input.Validate() != nil {
		return UploadResult{}, &Error{Code: "storage_upload_failed"}
	}
	file, err := os.Open(path)
	if err != nil {
		code := "storage_upload_failed"
		if errors.Is(err, os.ErrPermission) {
			code = "storage_permission_denied"
		}
		return UploadResult{}, &Error{Code: code}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return UploadResult{}, &Error{Code: "storage_upload_failed"}
	}

	var httpClient *http.Client
	if u.client != nil {
		httpClient = u.client(destination)
	} else {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		transport := newObjectTransport(destination, net.DefaultResolver, dialer.DialContext)
		defer transport.CloseIdleConnections()
		httpClient = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	options := s3.Options{
		Region:                     destination.Region,
		BaseEndpoint:               aws.String(destination.Endpoint),
		Credentials:                credentials.NewStaticCredentialsProvider(input.AccessKeyID, input.SecretAccessKey, input.SessionToken),
		HTTPClient:                 objectHTTPClient{client: httpClient},
		UsePathStyle:               destination.PathStyle,
		RetryMaxAttempts:           1,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	endpoint, _ := url.Parse(destination.Endpoint) // 已在上方校验。
	if destination.Provider == "oss" && !objectOfficialOSS(endpoint.Hostname()) {
		options.EndpointResolverV2 = objectOSSResolver{base: s3.NewDefaultEndpointResolverV2(), endpoint: *endpoint}
	}
	client := s3.New(options)
	metadata := map[string]string{"sha256": sha256}
	if info.Size() <= objectMinimumPartSize {
		requestCtx, state := objectRequest(ctx)
		_, err := client.PutObject(requestCtx, &s3.PutObjectInput{
			Bucket: aws.String(destination.Bucket), Key: aws.String(key), Body: file,
			ContentLength: aws.Int64(info.Size()), Metadata: metadata,
		})
		return objectUploadError(state, err, true)
	}

	requestCtx, state := objectRequest(ctx)
	created, err := client.CreateMultipartUpload(requestCtx, &s3.CreateMultipartUploadInput{Bucket: aws.String(destination.Bucket), Key: aws.String(key), Metadata: metadata})
	if err != nil {
		return objectUploadError(state, err, false)
	}
	if created.UploadId == nil || *created.UploadId == "" {
		return UploadResult{}, &Error{Code: "storage_upload_failed"}
	}
	complete := false
	defer func() {
		if complete {
			return
		}
		// 清理不继承取消或截止时间，避免任务或请求被取消后，未完成的分段上传无法清除。
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = client.AbortMultipartUpload(cleanupCtx, &s3.AbortMultipartUploadInput{Bucket: aws.String(destination.Bucket), Key: aws.String(key), UploadId: created.UploadId})
	}()

	partSize := objectPartSize(info.Size())
	count := (info.Size() + partSize - 1) / partSize
	parts := make([]types.CompletedPart, 0, count)
	for offset, number := int64(0), int32(1); offset < info.Size(); offset, number = offset+partSize, number+1 {
		length := min(partSize, info.Size()-offset)
		requestCtx, state = objectRequest(ctx)
		part, err := client.UploadPart(requestCtx, &s3.UploadPartInput{
			Bucket: aws.String(destination.Bucket), Key: aws.String(key), UploadId: created.UploadId,
			PartNumber: aws.Int32(number), Body: io.NewSectionReader(file, offset, length), ContentLength: aws.Int64(length),
		})
		if err != nil {
			return objectUploadError(state, err, false)
		}
		if part.ETag == nil || *part.ETag == "" {
			return UploadResult{}, &Error{Code: "storage_upload_failed"}
		}
		parts = append(parts, types.CompletedPart{ETag: part.ETag, PartNumber: aws.Int32(number)})
	}
	requestCtx, state = objectRequest(ctx)
	_, err = client.CompleteMultipartUpload(requestCtx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(destination.Bucket), Key: aws.String(key), UploadId: created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return objectUploadError(state, err, true)
	}
	complete = true
	return UploadResult{}, nil
}
