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
	"net/url"
	"strconv"
	"strings"
	"time"

	"orbitbeacon/internal/server/egress"
)

const SendTimeout = 10 * time.Second

type Payload struct {
	EventID string `json:"event_id"`
	State   string `json:"state"`
	Rule    struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Metric   string `json:"metric"`
		Severity string `json:"severity"`
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

// ParsePayload is the common persisted delivery decoder. Historical entity
// references are snapshots, not live dependencies or a source of new sends.
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
	// Deliberately unexported: only same-package tests may override Telegram/TLS.
	telegramBase string
	now          func() time.Time
}

func newSender(opts Options) *sender {
	d := &net.Dialer{Timeout: SendTimeout}
	safe := &egress.Dialer{Resolver: net.DefaultResolver, AllowPrivate: opts.AllowPrivateNetworks, DenyLocal: false, Dial: d.DialContext}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	// Avoid idle-connection diagnostics in net/http logging unsolicited remote bytes.
	transport := &http.Transport{DisableKeepAlives: true, Proxy: nil, DialContext: safe.DialContext, TLSClientConfig: tlsCfg, TLSHandshakeTimeout: SendTimeout, ResponseHeaderTimeout: SendTimeout, MaxIdleConns: 8, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}
	return &sender{allowInsecureWebhooks: opts.AllowInsecureWebhooks, client: &http.Client{Timeout: SendTimeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, dialer: safe, tlsConfig: tlsCfg, telegramBase: "https://api.telegram.org", now: opts.Now}
}
func (s *sender) Send(ctx context.Context, c Channel, secret string, p Payload) Result {
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	if c.Kind == "email" {
		return s.email(ctx, c, secret, p)
	}
	raw, e := json.Marshal(p)
	if e != nil {
		return failure("invalid_payload", false)
	}
	target := c.Config.URL
	if c.Kind == "webhook" {
		u, err := url.Parse(target)
		if err != nil {
			return failure("invalid_destination", false)
		}
		// Recheck current policy when sending persisted channels after config changes.
		if u.Scheme != "https" {
			ip := net.ParseIP(u.Hostname())
			if u.Scheme != "http" || !s.allowInsecureWebhooks || ip == nil || !ip.IsLoopback() {
				return failure("insecure_webhook_denied", false)
			}
		}
	}
	if c.Kind == "telegram" {
		target = s.telegramBase + "/bot" + secret + "/sendMessage"
		raw, _ = json.Marshal(struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}{c.Config.ChatID, message(p)})
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if e != nil {
		return failure("invalid_destination", false)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Kind == "webhook" {
		req.Header.Set("X-OrbitBeacon-Timestamp", strconv.FormatInt(s.now().Unix(), 10))
		if secret != "" {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(raw)
			req.Header.Set("X-OrbitBeacon-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
	}
	response, e := s.client.Do(req)
	if e != nil {
		return networkFailure(e)
	}
	defer response.Body.Close()
	// Bodies may contain credentials and are never returned, stored or logged.
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if readErr != nil {
			return networkFailure(readErr)
		}
		if c.Kind == "telegram" {
			var result struct {
				OK bool `json:"ok"`
			}
			if json.Unmarshal(body, &result) != nil || !result.OK {
				return failure("telegram_rejected", false)
			}
		}
		return Result{OK: true}
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
func message(p Payload) string {
	if p.SummaryCount > 1 {
		return fmt.Sprintf("OrbitBeacon: %d alert notifications coalesced. Latest: %s: %s on %s (%s, %s)", p.SummaryCount, p.State, p.Rule.Name, p.Node.Name, p.Rule.Metric, p.Rule.Severity)
	}
	value := ""
	if p.Value != nil {
		value = fmt.Sprintf(" value=%g", *p.Value)
	}
	return fmt.Sprintf("OrbitBeacon %s: %s on %s (%s, %s)%s\nEvent: %s\nStarted: %s", p.State, p.Rule.Name, p.Node.Name, p.Rule.Metric, p.Rule.Severity, value, p.EventID, p.StartedAt.UTC().Format(time.RFC3339))
}
