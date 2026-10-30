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

// LiveIdentity is an internal fence and is never serialized to a browser.
// The epoch also rejects credential ABA after an old request was admitted.
type LiveIdentity struct {
	Credential      [32]byte
	CredentialEpoch int64
	Generation      uint64
	Session         string
	HasCredential   bool
}

// LiveNode is the complete, deliberately compact fleet wire record.
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

// Presence is shared by durable inventory and identity-matched live overlays.
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

// LiveRecords reads the entire bounded inventory in one query. SQLite extracts
// the sole system field needed by fleet; no report, runtime or notes is decoded.
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

// The ordinary node scanner remains unchanged; only detail appends its private
// identity destinations to the same atomic SELECT.
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
