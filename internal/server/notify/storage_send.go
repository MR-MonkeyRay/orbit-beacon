package notify

import (
	"context"
	"fmt"
	"time"
)

// StoragePayload 仅包含故障事件元数据，不引用业务实体或通道凭据。
type StoragePayload struct {
	Kind       string    `json:"kind"`
	EventID    string    `json:"event_id"`
	Domain     string    `json:"domain"`
	Transition string    `json:"transition"`
	ActiveRole *string   `json:"active_role"`
	ReasonCode string    `json:"reason_code"`
	OccurredAt time.Time `json:"occurred_at"`
}

func messageLimit(kind string) int {
	if limits, ok := imChannels[kind]; ok {
		return limits.maxBytes
	}
	if kind == "telegram" {
		return 4096
	}
	return 0
}

func storageSeverity(transition string) string {
	switch transition {
	case "manual_required", "recovery_failed":
		return "critical"
	case "primary_restored":
		return "info"
	default:
		return "warning"
	}
}

func renderStorageMessage(p StoragePayload) string {
	role := "none"
	if p.ActiveRole != nil {
		role = *p.ActiveRole
	}
	return fmt.Sprintf("OrbitBeacon storage: %s\nDomain: %s\nActive role: %s\nReason: %s\nEvent: %s\nOccurred: %s", p.Transition, p.Domain, role, p.ReasonCode, p.EventID, p.OccurredAt.UTC().Format(time.RFC3339Nano))
}

func (s *sender) SendStorage(ctx context.Context, c Channel, secret string, p StoragePayload) Result {
	return s.sendStorage(ctx, c, secret, p, nil)
}

func (s *sender) sendStorage(ctx context.Context, c Channel, secret string, p StoragePayload, current func(context.Context) (bool, error)) Result {
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	if !s.reserveSlot(ctx) {
		return networkFailure(ctx.Err())
	}
	defer func() { <-s.slots }()
	if current != nil {
		ok, err := current(ctx)
		if err != nil || !ok {
			return failure("snapshot_paused", true)
		}
	}
	p.Kind = "storage"
	text := ""
	if c.Kind != "webhook" {
		text = renderStorageMessage(p)
	}
	if c.Kind == "email" {
		return s.emailText(ctx, c, secret, "OrbitBeacon storage notification", text)
	}
	req, code := s.messageRequest(ctx, c, secret, p, text, storageSeverity(p.Transition))
	if code != "" {
		return failure(code, false)
	}
	return s.sendHTTP(req, c.Kind)
}
