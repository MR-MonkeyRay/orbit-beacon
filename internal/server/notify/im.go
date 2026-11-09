package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type imChannel struct {
	maxBytes int
	hosts    []string
	path     string
	query    string
	limited  map[int]time.Duration
}

var imChannels = map[string]imChannel{
	"feishu":   {maxBytes: 16000, hosts: []string{"open.feishu.cn", "open.larksuite.com"}, path: "/open-apis/bot/v2/hook/", limited: map[int]time.Duration{11232: time.Minute}},
	"dingtalk": {maxBytes: 4000, hosts: []string{"oapi.dingtalk.com"}, path: "/robot/send", query: "access_token", limited: map[int]time.Duration{410100: 10 * time.Minute, 130101: 10 * time.Minute}},
	"wecom":    {maxBytes: 2048, hosts: []string{"qyapi.weixin.qq.com"}, path: "/cgi-bin/webhook/send", query: "key", limited: map[int]time.Duration{45009: time.Minute}},
	"bark":     {maxBytes: 4000},
	"ntfy":     {maxBytes: 4096},
}

// UsesURL 让凭据存储与备份遵循同一通道分类，避免新增通道漏掉加密。
func UsesURL(kind string) bool {
	_, ok := imChannels[kind]
	return kind == "webhook" || ok
}

var topicSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validIMSecret(kind, secret string) bool {
	switch kind {
	case "wecom":
		return secret == ""
	case "bark":
		return len(secret) <= 128 && topicSegment.MatchString(secret)
	case "ntfy":
		// Bearer 令牌进入请求头，须在配置阶段拒绝控制字符。
		for _, r := range secret {
			if r < 32 || r == 127 {
				return false
			}
		}
	}
	return true
}

func destinationURL(kind, raw string, allowInsecure bool) (*url.URL, string) {
	u, err := url.Parse(raw)
	if err != nil || !validHost(u.Hostname()) || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.Opaque != "" || strings.ContainsAny(raw, "\r\n") || strings.HasSuffix(u.Host, ":") {
		return nil, "invalid_destination"
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, "invalid_destination"
		}
	}
	policy, im := imChannels[kind]
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !allowInsecure || ip == nil || !ip.IsLoopback() {
			return nil, "insecure_webhook_denied"
		}
	}
	if !im {
		return u, ""
	}
	if len(policy.hosts) > 0 && (u.Port() != "" || !slices.Contains(policy.hosts, u.Hostname())) {
		return nil, "invalid_destination"
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, "invalid_destination"
	}
	if policy.query != "" {
		values := query[policy.query]
		if len(query) != 1 || len(values) != 1 || values[0] == "" || u.Path != policy.path {
			return nil, "invalid_destination"
		}
	} else if u.RawQuery != "" || u.ForceQuery {
		return nil, "invalid_destination"
	}
	switch kind {
	case "feishu":
		if !strings.HasPrefix(u.Path, policy.path) || strings.TrimPrefix(u.Path, policy.path) == "" || strings.Contains(strings.TrimPrefix(u.Path, policy.path), "/") {
			return nil, "invalid_destination"
		}
	case "bark":
		if u.Path != "" && u.Path != "/" {
			return nil, "invalid_destination"
		}
	case "ntfy":
		if !strings.HasPrefix(u.Path, "/") {
			return nil, "invalid_destination"
		}
		for _, segment := range strings.Split(strings.TrimPrefix(u.Path, "/"), "/") {
			if !topicSegment.MatchString(segment) {
				return nil, "invalid_destination"
			}
		}
	}
	return u, ""
}

func (s *sender) imRequest(ctx context.Context, c Channel, secret string, p Payload) (*http.Request, string) {
	return s.imTextRequest(ctx, c, secret, renderMessage(p, c.Config.Template, imChannels[c.Kind].maxBytes), p.Rule.Severity)
}

func (s *sender) imTextRequest(ctx context.Context, c Channel, secret, text, severity string) (*http.Request, string) {
	// 配置与运行时策略可能变化，不能信任数据库中的旧 URL。
	u, code := destinationURL(c.Kind, c.Config.URL, s.allowInsecureWebhooks)
	if code != "" {
		return nil, code
	}
	var payload any
	switch c.Kind {
	case "feishu":
		body := map[string]any{"msg_type": "text", "content": map[string]string{"text": text}}
		if secret != "" {
			timestamp := strconv.FormatInt(s.now().Unix(), 10)
			mac := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
			body["timestamp"], body["sign"] = timestamp, base64.StdEncoding.EncodeToString(mac.Sum(nil))
		}
		payload = body
	case "dingtalk", "wecom":
		payload = map[string]any{"msgtype": "text", "text": map[string]string{"content": text}}
		if c.Kind == "dingtalk" && secret != "" {
			timestamp := strconv.FormatInt(s.now().UnixMilli(), 10)
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(timestamp + "\n" + secret))
			query := u.Query()
			query.Set("timestamp", timestamp)
			query.Set("sign", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			u.RawQuery = query.Encode()
		}
	case "bark":
		u.Path = "/push"
		u.RawPath = ""
		payload = map[string]string{"device_key": secret, "title": "OrbitBeacon", "body": text}
	}
	raw := []byte(text)
	if payload != nil {
		var err error
		raw, err = json.Marshal(payload)
		if err != nil {
			return nil, "invalid_payload"
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(raw))
	if err != nil {
		return nil, "invalid_destination"
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Kind == "ntfy" {
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		req.Header.Set("Title", "OrbitBeacon")
		priority := map[string]string{"critical": "5", "warning": "4"}[severity]
		if priority == "" {
			priority = "3"
		}
		req.Header.Set("Priority", priority)
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
	}
	return req, ""
}

func channelResponse(kind string, body []byte) Result {
	switch kind {
	case "telegram":
		var result struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal(body, &result) != nil || !result.OK {
			return failure("telegram_rejected", false)
		}
	case "feishu":
		return codeResponse(kind, body, "code")
	case "dingtalk", "wecom":
		return codeResponse(kind, body, "errcode")
	case "bark":
		// 响应结构没有官方说明，只把明确的失败码视为拒绝。
		var result struct {
			Code *float64 `json:"code"`
		}
		if json.Unmarshal(body, &result) == nil && result.Code != nil && *result.Code != 200 {
			return failure("channel_rejected", false)
		}
	}
	return Result{OK: true}
}

// 这些平台 HTTP 200 也可能是失败，缺少结果码时不能当作已送达。
func codeResponse(kind string, body []byte, field string) Result {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return failure("channel_rejected", false)
	}
	// 钉钉的 errcode 有字符串和数字两种写法。
	code, err := strconv.Atoi(strings.Trim(string(fields[field]), `"`))
	if err != nil {
		return failure("channel_rejected", false)
	}
	if code == 0 {
		return Result{OK: true}
	}
	// 钉钉和企业微信的限流码来自非官方或镜像资料，尚未实测。
	if delay := imChannels[kind].limited[code]; delay > 0 {
		result := failure("rate_limited", true)
		result.retryAfter = delay
		return result
	}
	return failure("channel_rejected", false)
}
