package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/validation"
)

func templatePayload() Payload {
	value := 12.5
	started := time.Date(2026, 11, 5, 12, 30, 0, 0, time.FixedZone("offset", 3600))
	resolved := started.Add(time.Minute)
	p := Payload{EventID: "event-1", State: "resolved", Value: &value, StartedAt: started, ResolvedAt: &resolved}
	p.Rule.Name, p.Rule.Metric, p.Rule.Severity = "CPU", "cpu_percent", "warning"
	p.Node.Name = "edge"
	return p
}

func TestRenderMessageTemplatesAndDefaults(t *testing.T) {
	p := templatePayload()
	const template = "{state}|{rule}|{node}|{metric}|{severity}|{value}|{started_at}|{resolved_at}|{event_id}"
	const want = "resolved|CPU|edge|cpu_percent|warning|12.5|2026-11-05T11:30:00Z|2026-11-05T11:31:00Z|event-1"
	if got := renderMessage(p, template, 0); got != want {
		t.Fatalf("template=%q want %q", got, want)
	}
	const original = "OrbitBeacon resolved: CPU on edge (cpu_percent, warning) value=12.5\nEvent: event-1\nStarted: 2026-11-05T11:30:00Z"
	if got := renderMessage(p, "", 0); got != original {
		t.Fatalf("default=%q want %q", got, original)
	}
	p.Node.Name = "{rule}"
	if got := renderMessage(p, "{node}/{rule}", 0); got != "{rule}/CPU" {
		t.Fatalf("recursive expansion: %q", got)
	}
	p.Node.Name = "edge"
	p.Value, p.ResolvedAt = nil, nil
	if got := renderMessage(p, "{value}|{resolved_at}", 0); got != "|" {
		t.Fatalf("nil expansion=%q", got)
	}
	const noValue = "OrbitBeacon resolved: CPU on edge (cpu_percent, warning)\nEvent: event-1\nStarted: 2026-11-05T11:30:00Z"
	if got := renderMessage(p, "", 0); got != noValue {
		t.Fatalf("nil default=%q want %q", got, noValue)
	}
	p.SummaryCount = 3
	const summary = "OrbitBeacon: 3 alert notifications coalesced. Latest: resolved: CPU on edge (cpu_percent, warning)"
	if got := renderMessage(p, "", 0); got != summary {
		t.Fatalf("summary=%q want %q", got, summary)
	}
	if got := renderMessage(p, "{node}\n{rule}", 0); got != "OrbitBeacon: 3 alert notifications coalesced.\nedge\nCPU" {
		t.Fatalf("template summary=%q", got)
	}
	p.SummaryCount = 0
	if got := renderMessage(p, "{A} {foo1} {} {", 0); got != "{A} {foo1} {} {" {
		t.Fatalf("literal braces=%q", got)
	}
}

func TestRenderMessageUTF8Truncation(t *testing.T) {
	p := templatePayload()
	p.Node.Name = strings.Repeat("界🙂", 1000)
	for _, template := range []string{"{node}", ""} {
		for _, limit := range []int{3, 4, 5, 6, 7, 8, 11, 4096} {
			got := renderMessage(p, template, limit)
			if len(got) > limit || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
				t.Fatalf("limit=%d bytes=%d valid=%t suffix=%t", limit, len(got), utf8.ValidString(got), strings.HasSuffix(got, "…"))
			}
		}
	}
	if got := renderMessage(p, "{node}", 6); got != "界…" {
		t.Fatalf("boundary=%q", got)
	}
	if got := renderMessage(p, "界🙂", 7); got != "界🙂" {
		t.Fatalf("exact boundary=%q", got)
	}
	if got := renderMessage(p, "{node}", 0); got != p.Node.Name {
		t.Fatal("unlimited message truncated")
	}
}

func TestValidateChannelTemplate(t *testing.T) {
	for _, tc := range []struct {
		name, template string
		valid          bool
	}{
		{"empty", "", true},
		{"all", "{state} {rule} {node} {metric} {severity} {value} {started_at} {resolved_at} {event_id}", true},
		{"literal braces", "{A} {foo1} {} {", true},
		{"unknown", "{unknown}", false},
		{"unknown after known", "{node} {unknown_token}", false},
		{"1000 bytes", strings.Repeat("界", 333) + "a", true},
		{"1001 bytes", strings.Repeat("界", 333) + "ab", false},
		{"newline", "one\ntwo", true},
		{"tab", "one\ttwo", false},
		{"CR", "one\rtwo", false},
		{"NUL", "one\x00two", false},
		{"DEL", "one\x7ftwo", false},
		{"unicode control", "one\u0085two", false},
		{"invalid UTF8", string([]byte{0xff}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, kind := range []string{"telegram", "email"} {
				cfg := Config{ChatID: "-123", Template: tc.template}
				secret := "123:token"
				if kind == "email" {
					cfg = Config{Host: "smtp.example.com", Port: 587, From: "a@example.com", To: []string{"b@example.com"}, Template: tc.template}
					secret = ""
				}
				err := ValidateChannel(Channel{Name: "channel", Kind: kind, Config: cfg}, secret, false)
				if (err == nil) != tc.valid {
					t.Fatalf("%s valid=%t err=%v", kind, tc.valid, err)
				}
				if err != nil {
					assertTemplateConfigError(t, err)
				}
			}
		})
	}
	if err := ValidateChannel(Channel{Name: "hook", Kind: "webhook", Config: Config{URL: "https://example.com", Template: "{node}"}}, "", false); err == nil {
		t.Fatal("webhook template accepted")
	} else {
		assertTemplateConfigError(t, err)
	}
}

func assertTemplateConfigError(t *testing.T, err error) {
	t.Helper()
	var invalid *validation.Error
	if !errors.As(err, &invalid) {
		t.Fatalf("not a validation error: %v", err)
	}
	if _, ok := invalid.Fields["config"]; !ok {
		t.Fatalf("missing config field: %+v", invalid)
	}
}

func TestChannelConfigRejectsInvalidUTF8BeforeDecoding(t *testing.T) {
	f := newFixture(t)
	enabled := true
	raw := []byte("{\"chat_id\":\"-123\",\"template\":\"a\xffb\"}")
	_, err := f.s.Save(audit.WithActor(context.Background(), "admin"), "", Patch{Name: ptr("t"), Kind: ptr("telegram"), Enabled: &enabled, Config: raw, Secret: json.RawMessage(`"123:token"`)})
	var invalid *validation.Error
	if !errors.As(err, &invalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestChannelTemplatePatchPersistenceAndTestSend(t *testing.T) {
	f := newFixture(t)
	ctx := audit.WithActor(context.Background(), "admin")
	for _, kind := range []string{"telegram", "email"} {
		t.Run(kind, func(t *testing.T) {
			cfg := Config{ChatID: "-123", Template: " initial {node} "}
			secret := json.RawMessage(`"123:token"`)
			if kind == "email" {
				cfg = Config{Host: "smtp.example.com", Port: 587, From: "a@example.com", To: []string{"b@example.com"}, Template: " initial {node} "}
				secret = nil
			}
			raw, _ := json.Marshal(cfg)
			enabled := true
			c, err := f.s.Save(ctx, "", Patch{Name: ptr(kind), Kind: ptr(kind), Enabled: &enabled, Config: raw, Secret: secret})
			if err != nil {
				t.Fatal(err)
			}
			if c.Config.Template != "initial {node}" {
				t.Fatalf("create normalization=%q", c.Config.Template)
			}
			cfg.Template = " \r\n{state}\r\n{event_id}\r\n "
			raw, _ = json.Marshal(cfg)
			if _, err = f.s.Save(ctx, c.ID, Patch{Config: raw}); err != nil {
				t.Fatal(err)
			}
			c, err = f.s.Get(ctx, c.ID)
			if err != nil || c.Config.Template != "{state}\n{event_id}" {
				t.Fatalf("read template=%q err=%v", c.Config.Template, err)
			}
			if _, err = f.s.Save(ctx, c.ID, Patch{Name: ptr("renamed")}); err != nil {
				t.Fatal(err)
			}
			c, _, err = f.s.load(ctx, c.ID)
			if err != nil || c.Config.Template != "{state}\n{event_id}" {
				t.Fatalf("load template=%q err=%v", c.Config.Template, err)
			}
			raw, err = json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				Config map[string]any `json:"config"`
			}
			if err = json.Unmarshal(raw, &response); err != nil || response.Config["template"] != "{state}\n{event_id}" {
				t.Fatalf("response=%s err=%v", raw, err)
			}
			if kind == "telegram" {
				calls := 0
				f.s.sender.client.Transport = webhookTestTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					var body struct {
						Text string `json:"text"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.Text != "test\ntest" {
						t.Errorf("test template=%q", body.Text)
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
				})
				result, err := f.s.Test(ctx, c.ID)
				if err != nil || !result.OK || calls != 1 {
					t.Fatalf("test=%+v err=%v calls=%d", result, err, calls)
				}
			}
			cfg.Template = " \t\r\n "
			raw, _ = json.Marshal(cfg)
			if _, err = f.s.Save(ctx, c.ID, Patch{Config: raw}); err != nil {
				t.Fatal(err)
			}
			c, err = f.s.Get(ctx, c.ID)
			if err != nil || c.Config.Template != "" {
				t.Fatalf("clear template=%q err=%v", c.Config.Template, err)
			}
			raw, _ = json.Marshal(c)
			if strings.Contains(string(raw), `"template"`) {
				t.Fatalf("empty template response=%s", raw)
			}
		})
	}
	for _, raw := range []string{`{"url":"https://example.com","template":"{node}"}`, `{"url":"https://example.com","template":""}`} {
		if _, err := f.s.Save(ctx, f.channel.ID, Patch{Config: json.RawMessage(raw)}); err == nil {
			t.Fatal("webhook template patch accepted")
		} else {
			assertTemplateConfigError(t, err)
		}
	}
}

func TestTelegramTemplateSendAndByteLimit(t *testing.T) {
	s := newSender(Options{Now: time.Now})
	defer s.client.CloseIdleConnections()
	p := templatePayload()
	c := Channel{Kind: "telegram", Config: Config{ChatID: "-123", Template: "{state}: {node} {value}"}}
	expected := "resolved: edge 12.5"
	calls := 0
	s.client.Transport = webhookTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["text"] != expected || body["chat_id"] != "-123" || len(body) != 2 || r.URL.Path != "/bot123:token/sendMessage" {
			t.Errorf("request body=%v path=%s", body, r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	})
	if result := s.Send(context.Background(), c, "123:token", p); !result.OK {
		t.Fatal(result)
	}
	c.Config.Template = "{node}"
	p.Node.Name = strings.Repeat("界", 2000)
	expected = strings.Repeat("界", 1364) + "…"
	if result := s.Send(context.Background(), c, "123:token", p); !result.OK {
		t.Fatal(result)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}
