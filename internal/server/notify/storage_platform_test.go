package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStorageIMUsesFixedTextAndExplicitSeverity(t *testing.T) {
	for kind, target := range imTargets {
		t.Run(kind, func(t *testing.T) {
			response := map[string]string{"feishu": `{"code":0}`, "dingtalk": `{"errcode":0}`, "wecom": `{"errcode":0}`, "bark": `{"code":200}`}[kind]
			inspected := false
			s := imTestSender(t, kind, func(request *http.Request) {
				inspected = true
				raw, _ := io.ReadAll(request.Body)
				if !strings.Contains(string(raw), "manual_required") || strings.Contains(string(raw), "attacker-template") {
					t.Error("storage payload used alert template")
				}
				if kind == "ntfy" && request.Header.Get("Priority") != "5" {
					t.Error("missing critical storage severity")
				}
			}, 200, response)
			result := s.SendStorage(context.Background(), Channel{Kind: kind, Config: Config{URL: target, Template: "attacker-template {node}"}}, imSecret(kind), storageTestPayload())
			if !result.OK || !inspected {
				t.Fatalf("send=%+v inspected=%t", result, inspected)
			}
		})
	}
}

func TestStorageTelegramUsesFixedTextWithoutAlertEntities(t *testing.T) {
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		if json.NewDecoder(r.Body).Decode(&message) != nil || message.ChatID != "-123" || !strings.Contains(message.Text, "manual_required") || strings.Contains(message.Text, "attacker-template") {
			t.Error("invalid storage Telegram text")
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer receiver.Close()
	s := newSender(Options{Now: time.Now, AllowPrivateNetworks: true})
	defer s.client.CloseIdleConnections()
	trustTestServer(s, receiver)
	s.telegramBase = receiver.URL
	result := s.SendStorage(context.Background(), Channel{Kind: "telegram", Config: Config{ChatID: "-123", Template: "attacker-template {node}"}}, "123:token", storageTestPayload())
	if !result.OK {
		t.Fatalf("send=%+v", result)
	}
}
