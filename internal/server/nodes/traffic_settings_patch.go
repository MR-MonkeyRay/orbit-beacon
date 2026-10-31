package nodes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// 原始字段在保留类型化服务 API 的同时保存显式 null。
// 使用与 HTTP 边界相同的未知字段策略解码；不要把外层对象交给默认宽松的
// json.Unmarshal 来绕过该策略。
func (p *TrafficSettingsPatch) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return errors.New("traffic settings patch must be an object")
	}
	var wire struct {
		BillingDay json.RawMessage `json:"billing_day"`
		QuotaBytes json.RawMessage `json:"quota_bytes"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	*p = TrafficSettingsPatch{}
	for _, field := range [...]struct {
		name string
		raw  json.RawMessage
		dst  any
	}{
		{"billing_day", wire.BillingDay, &p.BillingDay},
		{"quota_bytes", wire.QuotaBytes, &p.QuotaBytes},
	} {
		if len(field.raw) == 0 {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(field.raw), []byte("null")) {
			if p.invalidFields == nil {
				p.invalidFields = make(map[string]string)
			}
			p.invalidFields[field.name] = "must not be null"
			continue
		}
		if err := json.Unmarshal(field.raw, field.dst); err != nil {
			return fmt.Errorf("invalid traffic settings field %q: %w", field.name, err)
		}
	}
	return nil
}
