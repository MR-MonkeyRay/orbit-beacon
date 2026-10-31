package audit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"time"

	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

type actorKey struct{}

type systemActorKey struct{}

// WithActor 将变更归属于已认证的会话用户。
func WithActor(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, actorKey{}, userID)
}

// WithSystemActor 将非用户主体（如持注册票据的 Agent）发起的变更记入审计。
// 事件不带 actor_id，details 中记录主体类型。
func WithSystemActor(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, systemActorKey{}, kind)
}

// Details 记录变更的少量事实，不得包含密钥、令牌或密码类内容。
type Details map[string]any

// Insert 在变更事务内写入审计事件。未设置主体的内部初始化和测试数据操作不产生事件。
func Insert(ctx context.Context, tx *sql.Tx, action, targetID string, now time.Time) error {
	return InsertDetails(ctx, tx, action, targetID, now, nil)
}

// InsertDetails 同 Insert，附带结构化 details。
func InsertDetails(ctx context.Context, tx *sql.Tx, action, targetID string, now time.Time, details Details) error {
	var actor any
	if actorID, _ := ctx.Value(actorKey{}).(string); actorID != "" {
		actor = actorID
	} else if kind, _ := ctx.Value(systemActorKey{}).(string); kind != "" {
		if details == nil {
			details = Details{}
		}
		details["actor_kind"] = kind
	} else {
		return nil
	}
	encoded := []byte("{}")
	if len(details) != 0 {
		var err error
		if encoded, err = json.Marshal(details); err != nil {
			return err
		}
	}
	id, err := uuid.UUID(rand.Reader)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(id, actor_id, action, target_id, created_at, details) VALUES (?, ?, ?, ?, ?, ?)", id, actor, action, targetID, timekey.Format(now), string(encoded))
	return err
}

// ScopedAlertRuleDeletes 将清单作用域级联删除的规则归属于同一主体和事务。
func ScopedAlertRuleDeletes(ctx context.Context, tx *sql.Tx, kind, scopeID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM alert_rules WHERE scope_kind=? AND scope_id=?", kind, scopeID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := InsertDetails(ctx, tx, "alert_rule.delete", id, now, Details{"scope_kind": kind, "scope_id": scopeID}); err != nil {
			return err
		}
	}
	return nil
}
