package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"orbitbeacon/internal/timekey"
)

// Task is the credential-free task projection. Private fields are never JSON encoded.
type Task struct {
	ID                         string          `json:"id"`
	Kind                       string          `json:"kind"`
	State                      string          `json:"state"`
	Phase                      string          `json:"phase"`
	PlanID                     *string         `json:"plan_id"`
	ActorID                    *string         `json:"actor_id"`
	Categories                 []Category      `json:"categories"`
	Destination                TaskDestination `json:"destination"`
	ScheduledAt                *time.Time      `json:"scheduled_at"`
	CreatedAt                  time.Time       `json:"created_at"`
	StartedAt                  *time.Time      `json:"started_at"`
	FinishedAt                 *time.Time      `json:"finished_at"`
	ArtifactName               *string         `json:"artifact_name"`
	ArtifactBytes              *string         `json:"artifact_bytes"`
	ArtifactSHA256             *string         `json:"artifact_sha256"`
	ArtifactExpiresAt          *time.Time      `json:"artifact_expires_at"`
	ResultLocation             *string         `json:"result_location"`
	PasswordExpiresAt          *time.Time      `json:"password_expires_at"`
	ErrorCode                  string          `json:"error_code"`
	WarningCode                string          `json:"warning_code"`
	Restored                   bool            `json:"restored"`
	DownloadAvailable          bool            `json:"download_available"`
	PasswordAvailable          bool            `json:"password_available"`
	passwordCiphertext         []byte
	inputCredentialsCiphertext []byte
	artifactDeletedAt          *time.Time
	artifactCleanupAfter       *time.Time
}

type TaskDestination struct {
	PlanName  string          `json:"plan_name"`
	ActorName string          `json:"actor_name"`
	Target    json.RawMessage `json:"target"`
}

const taskSelect = `SELECT id,kind,state,phase,plan_id,actor_id,categories_json,destination_json,scheduled_at,created_at,started_at,finished_at,artifact_name,artifact_bytes,artifact_sha256,artifact_expires_at,result_location,password_expires_at,error_code,warning_code,restored,password_ciphertext,input_credentials_ciphertext,artifact_deleted_at,artifact_cleanup_after FROM backup_tasks`

type taskScanner interface{ Scan(...any) error }

func scanTask(row taskScanner) (Task, error) {
	var t Task
	var categories, destination, created string
	var scheduled, started, finished, expires, passwordExpires, deleted, cleanup sql.NullString
	var size sql.NullInt64
	err := row.Scan(&t.ID, &t.Kind, &t.State, &t.Phase, &t.PlanID, &t.ActorID, &categories, &destination, &scheduled, &created, &started, &finished, &t.ArtifactName, &size, &t.ArtifactSHA256, &expires, &t.ResultLocation, &passwordExpires, &t.ErrorCode, &t.WarningCode, &t.Restored, &t.passwordCiphertext, &t.inputCredentialsCiphertext, &deleted, &cleanup)
	if err != nil {
		return t, err
	}
	if err = json.Unmarshal([]byte(categories), &t.Categories); err != nil {
		return t, err
	}
	if err = json.Unmarshal([]byte(destination), &t.Destination); err != nil {
		return t, err
	}
	if t.CreatedAt, err = timekey.Parse(created); err != nil {
		return t, err
	}
	for _, pair := range []struct {
		src sql.NullString
		dst **time.Time
	}{{scheduled, &t.ScheduledAt}, {started, &t.StartedAt}, {finished, &t.FinishedAt}, {expires, &t.ArtifactExpiresAt}, {passwordExpires, &t.PasswordExpiresAt}, {deleted, &t.artifactDeletedAt}, {cleanup, &t.artifactCleanupAfter}} {
		if pair.src.Valid {
			at, e := timekey.Parse(pair.src.String)
			if e != nil {
				return t, e
			}
			*pair.dst = &at
		}
	}
	if size.Valid {
		n := strconv.FormatInt(size.Int64, 10)
		t.ArtifactBytes = &n
	}
	return t, nil
}
func (s *Service) loadTask(ctx context.Context, id string) (Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, taskSelect+` WHERE id=?`, id))
}
