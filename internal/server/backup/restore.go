package backup

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/server/storage"
)

type RestoreResult struct {
	ArchiveID    string
	RollbackPath string
}

// Restore 是离线、受锁保护的类别替换。只有通过校验、
// 已关闭的可信数据库才会进入可恢复的多文件提交协议。
func Restore(ctx context.Context, dataDir, input, password string, replace bool) (RestoreResult, error) {
	var result RestoreResult
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return result, err
	}
	lock, err := datadir.Lock(dir)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	if err = RecoverRestore(dir); err != nil {
		return result, err
	}
	if err = datadir.ValidateRestore(dir); err != nil {
		return result, err
	}
	empty, err := restoreTargetEmpty(dir)
	if err != nil {
		return result, err
	}
	if !empty && !replace {
		return result, &Error{Code: "restore_requires_replace"}
	}
	work, err := os.MkdirTemp(dir, ".restore-work-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(work)
	if err = securefs.ValidateDirectory(work); err != nil {
		return result, err
	}
	archiveDir := filepath.Join(work, "archive")
	manifest, err := ReadArchive(ctx, input, password, archiveDir)
	if err != nil {
		return result, err
	}
	selected := make(map[Category]bool, len(manifest.Categories))
	for _, c := range manifest.Categories {
		selected[c] = true
	}
	if empty && !selected[Configuration] {
		return result, &Error{Code: "configuration_required"}
	}
	var key []byte
	if selected[Configuration] {
		key, err = datadir.LoadKey(filepath.Join(archiveDir, "secrets.key"))
	} else {
		key, err = datadir.LoadKey(filepath.Join(dir, "secrets.key"))
	}
	if err != nil {
		return result, &Error{Code: "restore_secret_conflict", Err: err}
	}
	defer clear(key)
	sources := make(map[Category]*sql.DB, 3)
	for _, category := range manifest.Categories {
		payload, e := openPayload(ctx, filepath.Join(archiveDir, string(category)+".sqlite"))
		if e != nil {
			return result, invalidPayload(string(category), "")
		}
		defer payload.Close()
		if e = ValidatePayloadSchema(ctx, payload, category); e == nil {
			e = ValidateSQLite(ctx, payload)
		}
		if e == nil {
			e = ValidatePayloadData(ctx, payload, category, key)
		}
		if e != nil {
			return result, &Error{Code: "restore_invalid_data", Err: e}
		}
		sources[category] = payload
	}
	var target, metrics *sql.DB
	if !empty && len(selected) < 3 {
		target, err = restoreTargetSnapshot(ctx, dir, work, "orbitbeacon.db", storage.CurrentSchemaVersion)
		if err != nil {
			return result, err
		}
		defer target.Close()
		if !selected[Monitoring] {
			metrics, err = restoreTargetSnapshot(ctx, dir, work, "metrics.db", history.CurrentSchemaVersion)
			if err != nil {
				return result, err
			}
			defer metrics.Close()
		}
		for _, category := range []Category{Configuration, Monitoring, Events} {
			if !selected[category] {
				sources[category] = target
			}
		}
	}
	newDir := filepath.Join(work, "new")
	if err = os.Mkdir(newDir, 0700); err != nil {
		return result, err
	}
	mainPath := filepath.Join(newDir, "orbitbeacon.db")
	metricsPath := filepath.Join(newDir, "metrics.db")
	main, err := storage.Open(ctx, mainPath)
	if err != nil {
		return result, err
	}
	defer main.Close()
	main.SetMaxOpenConns(1)
	historyDB, err := history.OpenDatabase(ctx, metricsPath)
	if err != nil {
		return result, err
	}
	defer historyDB.Close()
	historyDB.SetMaxOpenConns(1)
	now := time.Now().UTC()
	if err = buildRestoreState(ctx, main, historyDB, sources, metrics, selected, key, dir, manifest.ArchiveID, now); err != nil {
		return result, err
	}
	// 使用相同的可移植契约校验所选和保留的逻辑行。
	// 这些私有投影绝不安装或发布。
	for _, category := range []Category{Configuration, Monitoring, Events} {
		payload, e := createPayload(ctx, filepath.Join(work, "final-"+string(category)+".sqlite"), category)
		if e != nil {
			return result, e
		}
		e = projectCategory(ctx, payload, main, historyDB, category, now)
		if e == nil {
			e = ValidatePayloadSchema(ctx, payload, category)
		}
		if e == nil {
			e = ValidatePayloadData(ctx, payload, category, key)
		}
		e = errors.Join(e, payload.Close())
		if e != nil {
			return result, e
		}
	}
	if err = ValidateSQLite(ctx, main); err != nil {
		return result, err
	}
	if err = ValidateSQLite(ctx, historyDB); err != nil {
		return result, err
	}
	if err = closeRestoreDatabase(ctx, main, mainPath); err != nil {
		return result, err
	}
	if err = closeRestoreDatabase(ctx, historyDB, metricsPath); err != nil {
		return result, err
	}
	if err = writeRestoreKey(filepath.Join(newDir, "secrets.key"), key); err != nil {
		return result, err
	}
	rollback, err := CommitRestore(ctx, dir, newDir)
	if err != nil {
		return result, err
	}
	return RestoreResult{ArchiveID: manifest.ArchiveID, RollbackPath: rollback}, nil
}

func restoreTargetEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "server.lock" {
			continue
		}
		return false, nil
	}
	return true, nil
}

// 打开之前先复制完整的 SQLite 文件组。恢复/checkpoint 活动
// 仅允许作用于此私有副本，绝不允许作用于唯一的生产文件。
func restoreTargetSnapshot(ctx context.Context, dir, work, name string, version int) (*sql.DB, error) {
	raw := filepath.Join(work, "raw-"+name)
	if err := os.Mkdir(raw, 0700); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		source := filepath.Join(dir, name+suffix)
		exists, err := securefs.ValidateFile(source)
		if err != nil {
			return nil, &Error{Code: "restore_invalid_data", Err: err}
		}
		if !exists {
			if suffix == "" {
				return nil, &Error{Code: "restore_invalid_data", Err: os.ErrNotExist}
			}
			continue
		}
		if err = copyRestoreFile(ctx, source, filepath.Join(raw, name+suffix)); err != nil {
			return nil, err
		}
	}
	snapshot := filepath.Join(work, "target-"+name)
	if err := SnapshotDatabase(ctx, filepath.Join(raw, name), snapshot, version); err != nil {
		return nil, &Error{Code: "restore_invalid_data", Err: err}
	}
	db, err := openPayload(ctx, snapshot)
	if err != nil {
		return nil, &Error{Code: "restore_invalid_data", Err: err}
	}
	if err = ValidateSQLite(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func copyRestoreFile(ctx context.Context, source, destination string) error {
	in, err := securefs.OpenFile(source, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := securefs.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, contextReader{ctx: ctx, reader: in})
	return errors.Join(copyErr, out.Sync(), out.Close())
}
func writeRestoreKey(path string, key []byte) error {
	file, err := securefs.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, writeErr := file.Write(key)
	if writeErr == nil && n != len(key) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, file.Sync(), file.Close())
}
func closeRestoreDatabase(ctx context.Context, db *sql.DB, path string) error {
	var busy, log, checkpointed int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return &Error{Code: "restore_invalid_data"}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return &Error{Code: "restore_invalid_data"}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	file, err := securefs.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
