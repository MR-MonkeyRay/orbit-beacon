package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var imTargets = map[string]string{
	"feishu":   "https://open.feishu.cn/open-apis/bot/v2/hook/url-token",
	"dingtalk": "https://oapi.dingtalk.com/robot/send?access_token=url-token",
	"wecom":    "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=url-token",
	"bark":     "https://bark.example", "ntfy": "https://ntfy.example/topic/sub_topic",
}

func imSecret(kind string) string {
	if kind == "wecom" {
		return ""
	}
	return "device-secret"
}

func TestIMURLValidation(t *testing.T) {
	for kind, target := range imTargets {
		t.Run(kind, func(t *testing.T) {
			check := func(raw string, allow, want bool) {
				t.Helper()
				err := ValidateChannel(Channel{Name: "channel", Kind: kind, Config: Config{URL: raw}}, imSecret(kind), allow)
				if (err == nil) != want {
					t.Fatalf("URL policy mismatch: %v", err)
				}
				if err != nil && strings.Contains(err.Error(), "url-token") {
					t.Fatal("credential in error")
				}
			}
			check(target, false, true)
			u, _ := url.Parse(target)
			// 固定域名通道的白名单包含端口，自建通道允许自定义端口。
			fixed := len(imChannels[kind].hosts) > 0
			ported := *u
			ported.Host = u.Hostname() + ":8443"
			check(ported.String(), false, !fixed)
			for _, mutate := range []func(*url.URL){
				func(u *url.URL) { u.Scheme = "http" }, func(u *url.URL) { u.User = url.UserPassword("user", "password") },
				func(u *url.URL) { u.Fragment = "fragment" }, func(u *url.URL) { u.Host = "bad_host" },
				func(u *url.URL) { u.Host = u.Hostname() + ":0" }, func(u *url.URL) { u.Host = u.Hostname() + ":65536" },
				func(u *url.URL) { u.RawQuery += "&extra=1" }, func(u *url.URL) { u.Path = "/wrong/path!" },
			} {
				v := *u
				mutate(&v)
				check(v.String(), false, false)
			}
			check(target+"#", false, false)
			check(target+"\r\n", false, false)
			if kind == "feishu" {
				check(strings.Replace(target, "open.feishu.cn", "open.larksuite.com", 1), false, true)
				check(strings.TrimSuffix(target, "url-token"), false, false)
				check(strings.Replace(target, "open.feishu.cn", "example.com", 1), false, false)
			}
			if kind == "dingtalk" || kind == "wecom" {
				check(strings.TrimSuffix(target, "url-token"), false, false)
				check(target+"&"+map[string]string{"dingtalk": "access_token", "wecom": "key"}[kind]+"=two", false, false)
				check(strings.Replace(target, u.Hostname(), "example.com", 1), false, false)
			}
			if kind == "bark" || kind == "ntfy" {
				path := ""
				if kind == "ntfy" {
					path = "/topic"
				}
				check("http://127.0.0.1:8080"+path, false, false)
				check("http://127.0.0.1:8080"+path, true, true)
				check("http://example.com"+path, true, false)
			}
			if kind == "bark" {
				check(target+"/", false, true)
				check(target+"?", false, false)
			}
			if kind == "ntfy" {
				for _, path := range []string{"", "/", "/a/", "/a//b", "/a.b", "/a%2Fb!"} {
					check("https://ntfy.example"+path, false, false)
				}
			}
		})
	}
}
func TestIMSecretValidation(t *testing.T) {
	for _, tt := range []struct {
		kind, secret string
		valid        bool
	}{
		{"wecom", "", true}, {"wecom", "secret", false}, {"bark", "", false}, {"bark", "abc_-123", true},
		{"bark", strings.Repeat("a", 128), true}, {"bark", strings.Repeat("a", 129), false}, {"bark", "a/b", false}, {"bark", "设备", false},
		{"feishu", "", true}, {"dingtalk", "", true}, {"ntfy", "", true}, {"ntfy", "bad\r\ntoken", false},
	} {
		t.Run(tt.kind+strconv.Itoa(len(tt.secret)), func(t *testing.T) {
			err := ValidateChannel(Channel{Name: "channel", Kind: tt.kind, Config: Config{URL: imTargets[tt.kind]}}, tt.secret, false)
			if (err == nil) != tt.valid {
				t.Fatalf("secret policy mismatch: %v", err)
			}
		})
	}
}

func imTestSender(t *testing.T, kind string, inspect func(*http.Request), status int, body string) *sender {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status == http.StatusFound {
			w.Header().Set("Location", "https://redirect.example/private-token")
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	target, _ := url.Parse(ts.URL)
	s := newSender(Options{Now: func() time.Time { return time.Unix(1700000000, 123000000) }})
	transport := ts.Client().Transport
	t.Cleanup(transport.(*http.Transport).CloseIdleConnections)
	s.client.Transport = webhookTestTransport(func(r *http.Request) (*http.Response, error) {
		original, _ := url.Parse(imTargets[kind])
		if r.URL.Host != original.Host || r.URL.Scheme != "https" {
			t.Error("original host changed")
		}
		path := original.Path
		if kind == "bark" {
			path = "/push"
		}
		if r.URL.Path != path {
			t.Error("original path changed")
		}
		for key, values := range original.Query() {
			if r.URL.Query().Get(key) != values[0] {
				t.Error("original query changed")
			}
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if inspect != nil {
			inspect(r)
		}
		// inspect 可能读完请求体，转发前必须恢复，否则 ContentLength 不匹配。
		r.Body = io.NopCloser(bytes.NewReader(raw))
		forwarded := r.Clone(r.Context())
		v := *r.URL
		forwarded.URL = &v
		forwarded.URL.Host = target.Host
		return transport.RoundTrip(forwarded)
	})
	return s
}

func TestIMSignaturesAndBodies(t *testing.T) {
	for kind := range imTargets {
		for _, signed := range []bool{false, true} {
			if kind == "bark" && !signed {
				continue
			}
			t.Run(fmt.Sprintf("%s/%v", kind, signed), func(t *testing.T) {
				secret := ""
				if signed {
					secret = imSecret(kind)
				}
				// 固定输入使编码断言覆盖加号、斜杠与等号。
				if kind == "dingtalk" && signed {
					secret = "signature-secret-5"
				}
				s := imTestSender(t, kind, func(r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewReader(raw))
					if r.Method != "POST" {
						t.Error("wrong method")
					}
					if kind == "ntfy" {
						if string(raw) != "node-name" || r.Header.Get("Title") != "OrbitBeacon" || r.Header.Get("Priority") != "3" || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
							t.Error("bad ntfy request")
						}
						want := ""
						if secret != "" {
							want = "Bearer " + secret
						}
						if r.Header.Get("Authorization") != want {
							t.Error("bad authorization")
						}
						return
					}
					var body map[string]json.RawMessage
					if json.Unmarshal(raw, &body) != nil {
						t.Fatal("invalid body")
					}
					if r.Header.Get("Content-Type") != "application/json" {
						t.Error("wrong content type")
					}
					var text string
					switch kind {
					case "feishu":
						var content struct {
							Text string `json:"text"`
						}
						json.Unmarshal(body["content"], &content)
						text = content.Text
						if string(body["msg_type"]) != `"text"` {
							t.Error("wrong message type")
						}
						if secret == "" {
							if body["timestamp"] != nil || body["sign"] != nil {
								t.Error("unexpected signature")
							}
						} else {
							timestamp := strconv.FormatInt(safeFixedTime().Unix(), 10)
							key := []byte(timestamp + "\n" + secret)
							mac := hmac.New(sha256.New, key)
							mac.Write([]byte{})
							want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
							var sign, stamp string
							json.Unmarshal(body["sign"], &sign)
							json.Unmarshal(body["timestamp"], &stamp)
							if sign != want || stamp != timestamp {
								t.Error("wrong feishu empty-message signature")
							}
							wrong := hmac.New(sha256.New, []byte(secret))
							wrong.Write(key)
							if sign == base64.StdEncoding.EncodeToString(wrong.Sum(nil)) {
								t.Error("signature used concatenation as message")
							}
						}
					case "dingtalk", "wecom":
						var content struct {
							Content string `json:"content"`
						}
						json.Unmarshal(body["text"], &content)
						text = content.Content
						if string(body["msgtype"]) != `"text"` {
							t.Error("wrong message type")
						}
						if secret == "" {
							if r.URL.Query().Has("timestamp") || r.URL.Query().Has("sign") {
								t.Error("unexpected signature")
							}
						} else {
							timestamp := strconv.FormatInt(safeFixedTime().UnixMilli(), 10)
							mac := hmac.New(sha256.New, []byte(secret))
							mac.Write([]byte(timestamp + "\n" + secret))
							want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
							if r.URL.Query().Get("sign") != want || r.URL.Query().Get("timestamp") != timestamp || !strings.Contains(r.URL.RawQuery, "sign="+url.QueryEscape(want)) {
								t.Error("wrong dingtalk signature encoding")
							}
							if !strings.ContainsAny(want, "+") || !strings.Contains(want, "/") || !strings.Contains(want, "=") {
								t.Error("fixed signature must exercise + / =")
							}
						}
					case "bark":
						json.Unmarshal(body["body"], &text)
						if string(body["device_key"]) != strconv.Quote(secret) || string(body["title"]) != `"OrbitBeacon"` || body["level"] != nil || strings.Contains(r.URL.String(), secret) {
							t.Error("bad bark request")
						}
					}
					if text != "node-name" {
						t.Error("template not applied")
					}
				}, 200, map[string]string{"feishu": `{"code":0}`, "dingtalk": `{"errcode":0}`, "wecom": `{"errcode":0}`, "bark": `{"code":200}`}[kind])
				p := Payload{}
				p.Node.Name = "node-name"
				result := s.Send(context.Background(), Channel{Kind: kind, Config: Config{URL: imTargets[kind], Template: "{node}"}}, secret, p)
				if !result.OK {
					t.Fatalf("send failed: %+v", result)
				}
			})
		}
	}
}
func safeFixedTime() time.Time { return time.Unix(1700000000, 123000000) }

func TestIMResponses(t *testing.T) {
	for _, tt := range []struct {
		kind, body string
		ok         bool
		delay      time.Duration
	}{
		{"dingtalk", `{"errcode":0}`, true, 0}, {"dingtalk", `{"errcode":"0"}`, true, 0},
		{"dingtalk", `{"errcode":310000}`, false, 0}, {"dingtalk", `{"errcode":"310000"}`, false, 0},
		{"dingtalk", `{"errcode":410100}`, false, 10 * time.Minute}, {"dingtalk", `{"errcode":130101}`, false, 10 * time.Minute},
		{"dingtalk", `{"errcode":"410100"}`, false, 10 * time.Minute}, {"dingtalk", `{}`, false, 0}, {"dingtalk", `no JSON`, false, 0},
		{"feishu", `{"code":0}`, true, 0}, {"feishu", `{"code":19021}`, false, 0}, {"feishu", `{"code":11232}`, false, time.Minute}, {"feishu", `{}`, false, 0}, {"feishu", `no JSON`, false, 0},
		{"wecom", `{"errcode":0}`, true, 0}, {"wecom", `{"errcode":45009}`, false, time.Minute}, {"wecom", `{"errcode":93000}`, false, 0}, {"wecom", `{}`, false, 0}, {"wecom", `no JSON`, false, 0},
		{"bark", `{"code":200}`, true, 0}, {"bark", `{"code":400}`, false, 0}, {"bark", ``, true, 0}, {"bark", `no JSON`, true, 0},
		{"ntfy", ``, true, 0},
		{"feishu", `{"code":null}`, false, 0}, {"wecom", `{"errcode":null}`, false, 0}, {"dingtalk", `{"errcode":null}`, false, 0},
	} {
		t.Run(tt.kind+tt.body, func(t *testing.T) {
			s := imTestSender(t, tt.kind, nil, 200, tt.body)
			result := s.Send(context.Background(), Channel{Kind: tt.kind, Config: Config{URL: imTargets[tt.kind]}}, imSecret(tt.kind), Payload{})
			if result.OK != tt.ok || result.retry != (tt.delay > 0) || result.retryAfter != tt.delay {
				t.Fatalf("unexpected result %+v", result)
			}
			if !tt.ok {
				want := "channel_rejected"
				if tt.delay > 0 {
					want = "rate_limited"
				}
				if result.ErrorCode != want {
					t.Fatal(result)
				}
			}
		})
	}
}

func TestIMNtfyHeadersAndWecomLimit(t *testing.T) {
	for severity, priority := range map[string]string{"critical": "5", "warning": "4", "info": "3", "": "3"} {
		s := imTestSender(t, "ntfy", func(r *http.Request) {
			if r.Header.Get("Priority") != priority {
				t.Error("wrong priority")
			}
			for _, values := range r.Header {
				for _, value := range values {
					if strings.ContainsAny(value, "\r\n") || strings.Contains(value, "private-node") {
						t.Error("payload entered headers")
					}
				}
			}
		}, 200, "")
		p := Payload{}
		p.Rule.Severity = severity
		p.Node.Name = "private-node\r\nX-Evil: value"
		if result := s.Send(context.Background(), Channel{Kind: "ntfy", Config: Config{URL: imTargets["ntfy"]}}, "", p); !result.OK {
			t.Fatal(result)
		}
	}
	for _, size := range []int{2047, 2048, 2049, 2051} {
		s := imTestSender(t, "wecom", func(r *http.Request) {
			var body struct {
				Text struct {
					Content string `json:"content"`
				} `json:"text"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			text := body.Text.Content
			if !utf8.ValidString(text) || len(text) > 2048 {
				t.Error("invalid text boundary")
			}
			if size <= 2048 && len(text) != size {
				t.Error("unexpected truncation")
			}
			if size > 2048 && !strings.HasSuffix(text, "…") {
				t.Error("missing truncation suffix")
			}
		}, 200, `{"errcode":0}`)
		p := Payload{}
		p.Node.Name = strings.Repeat("界", size/3) + strings.Repeat("a", size%3)
		if result := s.Send(context.Background(), Channel{Kind: "wecom", Config: Config{URL: imTargets["wecom"], Template: "{node}"}}, "", p); !result.OK {
			t.Fatal(result)
		}
	}
}

func TestIMFailurePrivacyAndSendPolicy(t *testing.T) {
	for kind, target := range imTargets {
		for _, status := range []int{302, 400, 429, 500} {
			s := imTestSender(t, kind, nil, status, `{"message":"url-token device-secret"}`)
			result := s.Send(context.Background(), Channel{Kind: kind, Config: Config{URL: target}}, imSecret(kind), Payload{})
			if result.OK || result.retry != (status == 429 || status == 500) {
				t.Fatal(result)
			}
			if status == 302 && result.ErrorCode != "redirect_denied" {
				t.Fatal(result)
			}
			if strings.Contains(fmt.Sprintf("%+v", result), "url-token") || strings.Contains(fmt.Sprintf("%+v", result), "device-secret") {
				t.Fatal("credential in Result")
			}
			raw, _ := json.Marshal(Channel{Kind: kind, Config: Config{URL: target}})
			if bytes.Contains(raw, []byte("url-token")) {
				t.Fatal("credential in channel response")
			}
		}
		s := newSender(Options{Now: time.Now})
		s.client.Transport = webhookTestTransport(func(*http.Request) (*http.Response, error) {
			t.Error("invalid URL sent")
			return nil, fmt.Errorf("device-secret")
		})
		for _, raw := range []string{strings.Replace(target, "https:", "http:", 1), target + "#fragment", "https://bad_host/path"} {
			result := s.Send(context.Background(), Channel{Kind: kind, Config: Config{URL: raw}}, imSecret(kind), Payload{})
			if result.OK || result.retry || (result.ErrorCode != "insecure_webhook_denied" && result.ErrorCode != "invalid_destination") {
				t.Fatal(result)
			}
		}
	}
}

func TestIMSaveRetainsEncryptedCredentials(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for kind, target := range imTargets {
		t.Run(kind, func(t *testing.T) {
			enabled := true
			config, _ := json.Marshal(Config{URL: target, Template: "{node}"})
			secret, _ := json.Marshal(imSecret(kind))
			c, err := f.s.Save(ctx, "", Patch{Name: ptr("channel"), Kind: &kind, Enabled: &enabled, Config: config, Secret: secret})
			if err != nil {
				t.Fatal(err)
			}
			var encrypted, secretCipher []byte
			var raw string
			if err = f.db.QueryRow("SELECT url_ciphertext,secret_ciphertext,config_json FROM notification_channels WHERE id=?", c.ID).Scan(&encrypted, &secretCipher, &raw); err != nil {
				t.Fatal(err)
			}
			if len(encrypted) == 0 || bytes.Contains(encrypted, []byte(target)) || strings.Contains(raw, "url-token") || strings.Contains(raw, "device-secret") {
				t.Fatal("unencrypted URL")
			}
			plain, err := f.s.box.Open(c.ID+":url", encrypted)
			if err != nil || string(plain) != target {
				t.Fatal("bad URL encryption")
			}
			clear(plain)
			if imSecret(kind) != "" {
				plain, err = f.s.box.Open(c.ID, secretCipher)
				if err != nil || string(plain) != imSecret(kind) {
					t.Fatal("bad secret encryption")
				}
				clear(plain)
			}
			updated, err := f.s.Save(ctx, c.ID, Patch{Config: json.RawMessage(`{"template":"{rule}"}`)})
			if err != nil || !updated.Config.URLSet {
				t.Fatalf("PATCH failed: %v", err)
			}
			loaded, retained, err := f.s.load(ctx, c.ID)
			if err != nil || loaded.Config.URL != target || retained != imSecret(kind) || loaded.Config.Template != "{rule}" {
				t.Fatal("credentials not retained")
			}
			rawResponse, _ := json.Marshal(loaded)
			if bytes.Contains(rawResponse, []byte("url-token")) || bytes.Contains(rawResponse, []byte("device-secret")) {
				t.Fatal("credential leaked")
			}
			if _, err = f.s.Save(ctx, c.ID, Patch{Kind: ptr("email")}); err == nil {
				t.Fatal("kind changed")
			}
			invalidURL, _ := f.s.box.Seal(c.ID+":url", []byte("https://bad_host/invalid"))
			if _, err = f.db.Exec("UPDATE notification_channels SET url_ciphertext=? WHERE id=?", invalidURL, c.ID); err != nil {
				t.Fatal(err)
			}
			loaded, retained, err = f.s.load(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			result := f.s.sender.Send(ctx, loaded, retained, Payload{})
			if result.ErrorCode != "invalid_destination" || result.retry {
				t.Fatal(result)
			}
		})
	}
}

func TestIMRequestAndResponseContracts(t *testing.T) {
	s := newSender(Options{Now: safeFixedTime})
	for kind, target := range imTargets {
		secret := imSecret(kind)
		if kind == "dingtalk" {
			secret = "signature-secret-5"
		}
		p := Payload{}
		p.Node.Name = "private-node\r\nheader-injection"
		p.Rule.Severity = "critical"
		req, code := s.imRequest(context.Background(), Channel{Kind: kind, Config: Config{URL: target, Template: "{node}"}}, secret, p)
		if code != "" {
			t.Fatalf("%s: %s", kind, code)
		}
		body, _ := io.ReadAll(req.Body)
		req.Body.Close()
		var fields map[string]json.RawMessage
		if kind != "ntfy" && json.Unmarshal(body, &fields) != nil {
			t.Fatal("invalid JSON request")
		}
		switch kind {
		case "feishu":
			stamp := strconv.FormatInt(safeFixedTime().Unix(), 10)
			mac := hmac.New(sha256.New, []byte(stamp+"\n"+secret))
			mac.Write([]byte{})
			var sign string
			json.Unmarshal(fields["sign"], &sign)
			if sign != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
				t.Fatal("incorrect empty-message HMAC")
			}
		case "dingtalk":
			stamp := strconv.FormatInt(safeFixedTime().UnixMilli(), 10)
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(stamp + "\n" + secret))
			sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))
			if req.URL.Query().Get("sign") != sign || !strings.Contains(req.URL.RawQuery, "sign="+url.QueryEscape(sign)) || req.URL.Query().Get("access_token") != "url-token" {
				t.Fatal("incorrect encoded signature")
			}
		case "bark":
			if req.URL.Path != "/push" || strings.Contains(req.URL.String(), secret) || fields["level"] != nil {
				t.Fatal("incorrect Bark destination")
			}
		case "ntfy":
			if string(body) != p.Node.Name || req.Header.Get("Title") != "OrbitBeacon" || req.Header.Get("Priority") != "5" || req.Header.Get("Authorization") != "Bearer "+secret {
				t.Fatal("incorrect ntfy request")
			}
			for _, values := range req.Header {
				for _, v := range values {
					if strings.ContainsAny(v, "\r\n") || strings.Contains(v, "private-node") {
						t.Fatal("payload in headers")
					}
				}
			}
		}
	}
	for _, tt := range []struct {
		kind, body string
		ok         bool
		delay      time.Duration
	}{
		{"feishu", `{"code":0}`, true, 0}, {"feishu", `{"code":19021}`, false, 0}, {"feishu", `{"code":11232}`, false, time.Minute},
		{"dingtalk", `{"errcode":0}`, true, 0}, {"dingtalk", `{"errcode":"0"}`, true, 0}, {"dingtalk", `{"errcode":310000}`, false, 0}, {"dingtalk", `{"errcode":"310000"}`, false, 0},
		{"dingtalk", `{"errcode":410100}`, false, 10 * time.Minute}, {"dingtalk", `{"errcode":130101}`, false, 10 * time.Minute},
		{"wecom", `{"errcode":0}`, true, 0}, {"wecom", `{"errcode":45009}`, false, time.Minute}, {"wecom", `{"errcode":93000}`, false, 0},
		{"bark", `{"code":200}`, true, 0}, {"bark", `{"code":200.0}`, true, 0}, {"bark", `{"code":400.5}`, false, 0}, {"bark", ``, true, 0}, {"bark", `not JSON`, true, 0},
	} {
		result := channelResponse(tt.kind, []byte(tt.body))
		if result.OK != tt.ok || result.retry != (tt.delay > 0) || result.retryAfter != tt.delay {
			t.Fatalf("%s response: %+v", tt.kind, result)
		}
	}
	for _, kind := range []string{"feishu", "dingtalk", "wecom"} {
		for _, body := range []string{`{}`, `not JSON`, `{"code":null,"errcode":null}`} {
			result := channelResponse(kind, []byte(body))
			if result.OK || result.retry || result.ErrorCode != "channel_rejected" {
				t.Fatal(result)
			}
		}
	}
}
