package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
	"sync"
	"sync/atomic"
)

type Uploader interface {
	Upload(ctx context.Context, destination ObjectDestination, credentials ObjectCredentials, path, key, sha256 string) (UploadResult, error)
}
type UploadResult struct{ OutcomeUnknown bool }
type Options struct {
	DataDir       string
	SecretBox     *secretbox.Box
	ServerVersion string
	Now           func() time.Time
	// Location 在构造时捕获；nil 表示使用服务器进程所在时区。
	Location       *time.Location
	Random         io.Reader
	Uploader       Uploader
	removeArtifact func(string) error
	// 故障钩子仅作用于实例，且只检验简短的终态事务。
	beforeTerminal      func(*sql.Tx) error
	afterTerminalCommit func() error
}
type Service struct {
	db             *sql.DB
	box            *secretbox.Box
	opts           Options
	serverLocation *time.Location
	wake           chan struct{}
	running        atomic.Bool
	randomMu       sync.Mutex
	cleanupMu      sync.Mutex
	orphanDirs     *os.File
	orphanFiles    *os.File
	orphanTaskID   string
	localTempAfter string
}

func New(db *sql.DB, opts Options) (*Service, error) {
	if db == nil || opts.SecretBox == nil || !filepath.IsAbs(opts.DataDir) {
		return nil, &Error{Code: "secret_unreadable"}
	}
	if err := securefs.ValidateDirectory(opts.DataDir); err != nil {
		return nil, &Error{Code: "storage_permission_denied", Err: err}
	}
	key, err := datadir.LoadKey(filepath.Join(opts.DataDir, "secrets.key"))
	if err != nil {
		return nil, &Error{Code: "secret_unreadable", Err: err}
	}
	defer clear(key)
	expected, err := secretbox.New(key, "orbitbeacon backup secrets v1")
	if err != nil {
		return nil, &Error{Code: "secret_unreadable", Err: err}
	}
	probe := []byte("backup service key check")
	cipher, err := opts.SecretBox.Seal("service-key-check", probe)
	if err != nil {
		return nil, &Error{Code: "secret_unreadable", Err: err}
	}
	plain, err := expected.Open("service-key-check", cipher)
	defer clear(plain)
	if err != nil || !bytes.Equal(plain, probe) {
		return nil, &Error{Code: "secret_unreadable"}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	location := opts.Location
	if location == nil {
		location = time.Local
	}
	return &Service{db: db, box: opts.SecretBox, opts: opts, serverLocation: location, wake: make(chan struct{}, 1)}, nil
}
func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) CreateExport(ctx context.Context, actorID string, categories []Category) (Task, error) {
	selected, err := ValidateCategories(categories)
	if err != nil {
		return Task{}, err
	}
	if !uuid.ValidUUID(actorID) {
		return Task{}, &Error{Code: "invalid_actor"}
	}
	s.randomMu.Lock()
	id, err := uuid.UUID(s.opts.Random)
	s.randomMu.Unlock()
	if err != nil {
		return Task{}, &Error{Code: "internal_error", Err: err}
	}
	now := s.opts.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	// storage.Open 使用 BEGIN IMMEDIATE，容量检查与入队受同一写锁保护。
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM backup_tasks WHERE state IN ('queued','running')`).Scan(&count); err != nil {
		return Task{}, err
	}
	if count >= 32 {
		return Task{}, &Error{Code: "backup_queue_full"}
	}
	var actorName string
	if err = tx.QueryRowContext(ctx, `SELECT username FROM users WHERE id=?`, actorID).Scan(&actorName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, &Error{Code: "invalid_actor"}
		}
		return Task{}, err
	}
	destination := TaskDestination{ActorName: actorName, Target: json.RawMessage(`null`)}
	encoded, _ := json.Marshal(destination)
	cats, _ := json.Marshal(selected)
	if _, err = tx.ExecContext(ctx, `INSERT INTO backup_tasks(id,kind,state,actor_id,categories_json,destination_json,created_at) VALUES(?,'backup_export','queued',?,?,?,?)`, id, actorID, string(cats), string(encoded), timekey.Format(now)); err != nil {
		return Task{}, err
	}
	if err = audit.InsertDetails(audit.WithActor(ctx, actorID), tx, "backup.export", id, now, audit.Details{"categories": selected, "state": "queued"}); err != nil {
		return Task{}, err
	}
	if err = tx.Commit(); err != nil {
		return Task{}, err
	}
	s.signal()
	return Task{ID: id, Kind: "backup_export", State: "queued", ActorID: &actorID, Categories: selected, Destination: destination, CreatedAt: now}, nil
}
