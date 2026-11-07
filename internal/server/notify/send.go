package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"orbitbeacon/internal/server/egress"
)

const SendTimeout = 10 * time.Second

type Payload struct {
	EventID string `json:"event_id"`
	State   string `json:"state"`
	Rule    struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		Metric   string  `json:"metric"`
		Target   *string `json:"target,omitempty"`
		Severity string  `json:"severity"`
	} `json:"rule"`
	Node struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"node"`
	Value        *float64   `json:"value"`
	StartedAt    time.Time  `json:"started_at"`
	ResolvedAt   *time.Time `json:"resolved_at"`
	SummaryCount int        `json:"summary_count,omitempty"`
}

// ParsePayload 是持久化投递的公共解码器。其中的历史实体引用
// 只是快照，不是活跃依赖，也不会触发新的发送。
func ParsePayload(raw []byte) (Payload, error) {
	var p Payload
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return p, errors.New("invalid notification payload")
	}
	err := json.Unmarshal(raw, &p)
	return p, err
}

type Result struct {
	OK         bool   `json:"ok"`
	ErrorCode  string `json:"error_code"`
	retry      bool
	retryAfter time.Duration
}

func failure(code string, retry bool) Result { return Result{ErrorCode: code, retry: retry} }

type sender struct {
	allowInsecureWebhooks bool
	client                *http.Client
	dialer                *egress.Dialer
	tlsConfig             *tls.Config
	// 刻意不导出：仅同包测试可覆盖 Telegram/TLS。
	telegramBase string
	now          func() time.Time
}

func newSender(opts Options) *sender {
	d := &net.Dialer{Timeout: SendTimeout}
	safe := &egress.Dialer{Resolver: net.DefaultResolver, AllowPrivate: opts.AllowPrivateNetworks, DenyLocal: false, Dial: d.DialContext}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	// 避免 net/http 的空闲连接诊断把远端主动发来的字节写入日志。
	transport := &http.Transport{DisableKeepAlives: true, Proxy: nil, DialContext: safe.DialContext, TLSClientConfig: tlsCfg, TLSHandshakeTimeout: SendTimeout, ResponseHeaderTimeout: SendTimeout, MaxIdleConns: 8, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}
	return &sender{allowInsecureWebhooks: opts.AllowInsecureWebhooks, client: &http.Client{Timeout: SendTimeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, dialer: safe, tlsConfig: tlsCfg, telegramBase: "https://api.telegram.org", now: opts.Now}
}
func (s *sender) Send(ctx context.Context, c Channel, secret string, p Payload) Result {
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	if c.Kind == "email" {
		return s.email(ctx, c, secret, p)
	}
	req, e := s.buildRequest(ctx, c, secret, p)
	if e != "" {
		return failure(e, false)
	}
	response, err := s.client.Do(req)
	if err != nil {
		return networkFailure(err)
	}
	defer response.Body.Close()
	// 响应体可能包含凭据，绝不返回、存储或记录。
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if readErr != nil {
			return networkFailure(readErr)
		}
		return channelResponse(c.Kind, body)
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return failure("redirect_denied", false)
	}
	code := "http_rejected"
	retry := response.StatusCode == 429 || response.StatusCode >= 500
	if response.StatusCode == 429 {
		code = "rate_limited"
	}
	if response.StatusCode >= 500 {
		code = "remote_unavailable"
	}
	result := failure(code, retry)
	if retry {
		result.retryAfter = parseRetryAfter(response.Header.Get("Retry-After"), s.now())
	}
	return result
}
func (s *sender) buildRequest(ctx context.Context, c Channel, secret string, p Payload) (*http.Request, string) {
	if _, ok := imChannels[c.Kind]; ok {
		return s.imRequest(ctx, c, secret, p)
	}
	var raw []byte
	target := c.Config.URL
	switch c.Kind {
	case "webhook":
		// 配置变更后发送已持久化通道时，重新检查当前策略。
		if _, code := destinationURL(c.Kind, target, s.allowInsecureWebhooks); code != "" {
			return nil, code
		}
		var err error
		raw, err = json.Marshal(p)
		if err != nil {
			return nil, "invalid_payload"
		}
	case "telegram":
		target = s.telegramBase + "/bot" + secret + "/sendMessage"
		raw, _ = json.Marshal(struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}{c.Config.ChatID, renderMessage(p, c.Config.Template, 4096)})
	default:
		return nil, "invalid_destination"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return nil, "invalid_destination"
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Kind == "webhook" {
		timestamp := strconv.FormatInt(s.now().Unix(), 10)
		req.Header.Set("X-OrbitBeacon-Timestamp", timestamp)
		if secret != "" {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(timestamp + "."))
			mac.Write(raw)
			req.Header.Set("X-OrbitBeacon-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
		}
	}
	return req, ""
}

func parseRetryAfter(raw string, now time.Time) time.Duration {
	if n, e := strconv.ParseInt(raw, 10, 64); e == nil {
		if n <= 0 {
			return 0
		}
		if n >= 3600 {
			return time.Hour
		}
		return time.Duration(n) * time.Second
	}
	if t, e := http.ParseTime(raw); e == nil {
		return min(time.Hour, max(0, t.Sub(now)))
	}
	return 0
}
func networkFailure(e error) Result {
	if errors.Is(e, egress.ErrDenied) {
		return failure("destination_denied", false)
	}
	if errors.Is(e, context.Canceled) {
		return failure("canceled", true)
	}
	return failure("network_error", true)
}
func renderMessage(p Payload, template string, maxBytes int) string {
	value, resolved, target := "", "", ""
	if p.Rule.Target != nil {
		target = *p.Rule.Target
	}
	if p.Value != nil {
		value = fmt.Sprintf("%g", *p.Value)
	}
	if p.ResolvedAt != nil {
		resolved = p.ResolvedAt.UTC().Format(time.RFC3339)
	}
	started := p.StartedAt.UTC().Format(time.RFC3339)
	var text string
	if template != "" {
		text = strings.NewReplacer(
			"{state}", p.State, "{rule}", p.Rule.Name, "{node}", p.Node.Name,
			"{target}", target, "{metric}", p.Rule.Metric, "{severity}", p.Rule.Severity, "{value}", value,
			"{started_at}", started, "{resolved_at}", resolved, "{event_id}", p.EventID,
		).Replace(template)
		if p.SummaryCount > 1 {
			text = fmt.Sprintf("OrbitBeacon: %d alert notifications coalesced.\n%s", p.SummaryCount, text)
		}
	} else if p.SummaryCount > 1 {
		text = fmt.Sprintf("OrbitBeacon: %d alert notifications coalesced. Latest: %s: %s on %s (%s, %s)", p.SummaryCount, p.State, p.Rule.Name, p.Node.Name, p.Rule.Metric, p.Rule.Severity)
	} else {
		if p.Value != nil {
			value = " value=" + value
		}
		text = fmt.Sprintf("OrbitBeacon %s: %s on %s (%s, %s)%s\nEvent: %s\nStarted: %s", p.State, p.Rule.Name, p.Node.Name, p.Rule.Metric, p.Rule.Severity, value, p.EventID, started)
	}
	if template == "" && p.Rule.Target != nil {
		text += "\nTarget: " + target
	}
	if maxBytes > 0 && len(text) > maxBytes {
		const suffix = "…"
		end := maxBytes - len(suffix)
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		return text[:end] + suffix
	}
	return text
}
