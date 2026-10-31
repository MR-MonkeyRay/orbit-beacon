package nodes

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/timekey"
)

// LiveIdentity 是内部围栏，绝不序列化给浏览器。
// epoch 还能在旧请求被放行后拒绝凭据的 ABA 问题。
type LiveIdentity struct {
	Credential      [32]byte
	CredentialEpoch int64
	Generation      uint64
	Session         string
	HasCredential   bool
}

// LiveNode 是节点列表完整且紧凑的传输记录。
type LiveNode struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"`
	Presence     string     `json:"presence"`
	LastSeen     *time.Time `json:"last_seen"`
	AgentVersion string     `json:"agent_version"`
}

type LiveRecord struct {
	LiveNode
	Identity LiveIdentity `json:"-"`
}

type LiveDetailRecord struct {
	Node     Node
	Identity LiveIdentity `json:"-"`
	Runtime  *v1.RuntimeSettings
}

// Presence 由持久化清单和身份匹配的实时叠加层共用。
func Presence(status string, hasCredential bool, seen *time.Time, now time.Time) string {
	switch {
	case status == "disabled":
		return "disabled"
	case status == "pending" || !hasCredential:
		return "pending"
	case seen == nil:
		return "never"
	case seen.After(now) || now.Sub(*seen) > OnlineWindow:
		return "offline"
	default:
		return "online"
	}
}

func liveIdentity(credential []byte, epoch int64, generation, session string) (LiveIdentity, error) {
	identity := LiveIdentity{CredentialEpoch: epoch, Session: session, HasCredential: len(credential) != 0}
	if len(credential) != 0 && len(credential) != len(identity.Credential) {
		return identity, errors.New("invalid node credential")
	}
	copy(identity.Credential[:], credential)
	var err error
	identity.Generation, err = strconv.ParseUint(generation, 10, 64)
	return identity, err
}

// LiveRecords 用一条查询读取数量受限的完整节点清单。SQLite 只提取节点列表所需的
// 单个 system 字段；不解码 report、runtime 或 notes。
func (s *Service) LiveRecords(ctx context.Context) ([]LiveRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,status,last_seen,
		CASE WHEN json_valid(system) THEN COALESCE(json_extract(system,'$.agent_version'),'') ELSE '' END,
		token_hash,credential_epoch,run_generation,report_session_id FROM nodes ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]LiveRecord, 0)
	for rows.Next() {
		var record LiveRecord
		var seen sql.NullString
		var credential []byte
		var generation, session string
		var epoch int64
		if err := rows.Scan(&record.ID, &record.Status, &seen, &record.AgentVersion, &credential, &epoch, &generation, &session); err != nil {
			return nil, err
		}
		record.Identity, err = liveIdentity(credential, epoch, generation, session)
		if err != nil {
			return nil, err
		}
		if seen.Valid {
			timestamp, err := timekey.Parse(seen.String)
			if err != nil {
				return nil, err
			}
			record.LastSeen = &timestamp
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// 沿用普通节点扫描器，仅在详情读取时追加同一条 SELECT 返回的私有身份字段。
type liveDetailScanner struct {
	scanner
	credential *[]byte
	epoch      *int64
	generation *string
	session    *string
	runtime    *string
}

func (s liveDetailScanner) Scan(dest ...any) error {
	return s.scanner.Scan(append(dest, s.credential, s.epoch, s.generation, s.session, s.runtime)...)
}

func (s *Service) LiveDetail(ctx context.Context, id string) (LiveDetailRecord, error) {
	var record LiveDetailRecord
	var credential []byte
	var epoch int64
	var generation, session, runtime string
	row := s.db.QueryRowContext(ctx, "SELECT "+nodeColumns+",token_hash,credential_epoch,run_generation,report_session_id,agent_runtime FROM nodes WHERE id=?", id)
	node, err := scanNode(liveDetailScanner{row, &credential, &epoch, &generation, &session, &runtime}, s.now())
	if err != nil {
		return record, err
	}
	record.Node = node
	record.Identity, err = liveIdentity(credential, epoch, generation, session)
	if err != nil {
		return record, err
	}
	if record.Identity.HasCredential && record.Identity.Generation != 0 && record.Identity.Session != "" && runtime != "" {
		settings, err := v1.DecodeRuntimeSettings(runtime)
		if err != nil {
			return record, err
		}
		record.Runtime = &settings
	}
	return record, nil
}
