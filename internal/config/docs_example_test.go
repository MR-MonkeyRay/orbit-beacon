package config

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 运维会逐字复制 Agent 指南，因此其中的安装策略示例必须能在与
// ReadInstallPolicy 相同的严格规则下解码。
func TestAgentGuideInstallPolicyExampleDecodes(t *testing.T) {
	data, err := os.ReadFile("../../docs/agent.md")
	if err != nil {
		t.Fatal(err)
	}
	const start = "install-policy.json > /dev/null <<EOF\n"
	_, rest, found := strings.Cut(string(data), start)
	if !found {
		t.Fatal("agent guide no longer contains the install policy heredoc")
	}
	body, _, found := strings.Cut(rest, "\nEOF\n")
	if !found {
		t.Fatal("agent guide install policy heredoc is not terminated")
	}
	body = strings.NewReplacer("$AGENT_UID", "1000", "$AGENT_GID", "1000").Replace(body)
	decoder := json.NewDecoder(bytes.NewReader([]byte(body)))
	decoder.DisallowUnknownFields()
	var policy InstallPolicy
	if err := decoder.Decode(&policy); err != nil {
		t.Fatalf("agent guide install policy is not valid: %v", err)
	}
	if _, err := LoadAgent(LoadOptions{}, &policy); err != nil {
		t.Fatalf("agent guide install policy fails validation: %v", err)
	}
}
