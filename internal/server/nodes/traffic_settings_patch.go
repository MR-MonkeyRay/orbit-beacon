package nodes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Raw fields preserve explicit null while keeping the typed service API. Decode
// with the same unknown-field policy as the HTTP boundary; do not bypass it by
// delegating to the default permissive json.Unmarshal for the outer object.
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
