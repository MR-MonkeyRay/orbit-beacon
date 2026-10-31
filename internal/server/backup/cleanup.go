package backup

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

func cleanupLimit(limit int) int {
	if limit < 1 || limit > 500 {
		return 500
	}
	return limit
}

// Cleanup 执行有界的短数据库变更，删除文件期间绝不持有写事务。
// 计数包含延迟失败，因此调用方能在下一批越过永久不可访问的产物。
func (s *Service) Cleanup(ctx context.Context, now time.Time, limit int) (int64, error) {
	budget := cleanupLimit(limit)
	var count int64
	result, err := s.db.ExecContext(ctx, `UPDATE backup_tasks SET password_ciphertext=NULL WHERE id IN (SELECT id FROM backup_tasks WHERE state IN ('succeeded','failed') AND password_ciphertext IS NOT NULL AND password_expires_at IS NOT NULL AND password_expires_at<=? ORDER BY password_expires_at,id LIMIT ?)`, timekey.Format(now), budget)
	if err != nil {
		return count, err
	}
	n, err := result.RowsAffected()
	count += n
	budget -= int(n)
	if err != nil || budget == 0 {
		return count, err
	}
	rows, err := s.db.QueryContext(ctx, taskSelect+` WHERE kind='backup_export' AND restored=0 AND state IN ('succeeded','failed') AND artifact_deleted_at IS NULL AND artifact_cleanup_after<=? ORDER BY artifact_cleanup_after,id LIMIT ?`, timekey.Format(now), budget)
	if err != nil {
		return count, err
	}
	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return count, err
		}
		tasks = append(tasks, task)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return count, err
	}
	if closeErr != nil {
		return count, closeErr
	}
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		removeErr := s.removeTaskArtifact(task)
		if removeErr == nil || os.IsNotExist(removeErr) {
			_, err = s.db.ExecContext(ctx, `UPDATE backup_tasks SET artifact_deleted_at=?,artifact_cleanup_after=NULL WHERE id=? AND restored=0 AND state IN ('succeeded','failed')`, timekey.Format(now), task.ID)
		} else {
			_, err = s.db.ExecContext(ctx, `UPDATE backup_tasks SET artifact_cleanup_after=? WHERE id=? AND restored=0 AND state IN ('succeeded','failed')`, timekey.Format(now.Add(time.Hour)), task.ID)
		}
		if err != nil {
			return count, err
		}
		count++
		budget--
	}
	if budget == 0 {
		return count, nil
	}
	// 删除文件失败时，历史记录过期后仅保留固定的私有导出定位符。
	// 密文过期另行处理。
	result, err = s.db.ExecContext(ctx, `UPDATE backup_tasks SET plan_id=NULL,actor_id=NULL,categories_json='[]',destination_json='{}',input_credentials_ciphertext=NULL,scheduled_at=NULL,started_at=NULL,artifact_bytes=NULL,artifact_sha256=NULL,result_location=NULL,error_code='',warning_code='' WHERE id IN (SELECT id FROM backup_tasks WHERE state IN ('succeeded','failed') AND finished_at<=? AND kind='backup_export' AND restored=0 AND artifact_name IS NOT NULL AND artifact_deleted_at IS NULL AND (plan_id IS NOT NULL OR actor_id IS NOT NULL OR categories_json!='[]' OR destination_json!='{}' OR input_credentials_ciphertext IS NOT NULL OR scheduled_at IS NOT NULL OR started_at IS NOT NULL OR artifact_bytes IS NOT NULL OR artifact_sha256 IS NOT NULL OR result_location IS NOT NULL OR error_code!='' OR warning_code!='') ORDER BY finished_at,id LIMIT ?)`, timekey.Format(now.Add(-taskLifetime)), budget)
	if err != nil {
		return count, err
	}
	n, err = result.RowsAffected()
	count += n
	budget -= int(n)
	if err != nil || budget == 0 {
		return count, err
	}
	result, err = s.db.ExecContext(ctx, `DELETE FROM backup_tasks WHERE id IN (SELECT id FROM backup_tasks WHERE state IN ('succeeded','failed') AND finished_at<=? AND (kind!='backup_export' OR restored=1 OR artifact_name IS NULL OR artifact_deleted_at IS NOT NULL) ORDER BY finished_at,id LIMIT ?)`, timekey.Format(now.Add(-taskLifetime)), budget)
	if err != nil {
		return count, err
	}
	n, err = result.RowsAffected()
	return count + n, err
}

func (s *Service) removeTaskArtifact(task Task) error {
	if task.Restored || task.Kind != "backup_export" || !terminalTask(task) {
		return &Error{Code: "artifact_missing"}
	}
	if task.ArtifactName == nil {
		return nil
	}
	if !validArtifactName(*task.ArtifactName, task.ID) {
		return &Error{Code: "artifact_missing"}
	}
	dir, err := s.exportDirectory(task.ID)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, *task.ArtifactName)
	file, err := securefs.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	file.Close()
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0600 {
		return &Error{Code: "artifact_missing"}
	}
	if s.opts.removeArtifact != nil {
		err = s.opts.removeArtifact(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return err
	}
	return syncCleanupDirectory(dir)
}

func syncCleanupDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func openCleanupDirectory(path string) (*os.File, error) {
	if err := securefs.ValidateDirectory(path); err != nil {
		return nil, err
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := dir.Stat()
	entry, entryErr := os.Lstat(path)
	if err == nil {
		err = entryErr
	}
	if err == nil && (!info.IsDir() || !os.SameFile(info, entry)) {
		err = &Error{Code: "storage_permission_denied"}
	}
	if err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

func (s *Service) closeCleanup() {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	s.closeOrphanScan()
}

func (s *Service) closeOrphanScan() {
	if s.orphanFiles != nil {
		s.orphanFiles.Close()
		s.orphanFiles = nil
	}
	if s.orphanDirs != nil {
		s.orphanDirs.Close()
		s.orphanDirs = nil
	}
	s.orphanTaskID = ""
}

func ownArchiveEntry(name, id string, allowTemp bool) bool {
	if allowTemp && strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") {
		name = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp")
	}
	return validArtifactName(name, id)
}

// CleanupOrphans 在多次调用之间续跑有界的目录扫描。只有固定 UUID 的导出目录和
// 精确匹配的归档/临时文件名才符合条件。导入的任务历史刻意不接管本机文件的所有权。
func (s *Service) CleanupOrphans(ctx context.Context, limit int) (int64, error) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	root := filepath.Join(s.opts.DataDir, "exports")
	if s.orphanDirs == nil {
		dir, err := openCleanupDirectory(root)
		if os.IsNotExist(err) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		s.orphanDirs = dir
	}
	var count int64
	for range cleanupLimit(limit) {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		if s.orphanFiles == nil {
			entries, err := s.orphanDirs.ReadDir(1)
			if errors.Is(err, io.EOF) {
				s.closeOrphanScan()
				return count, nil
			}
			if err != nil {
				s.closeOrphanScan()
				return count, err
			}
			entry := entries[0]
			if !uuid.ValidUUID(entry.Name()) || !entry.IsDir() {
				continue
			}
			var active bool
			if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM backup_tasks WHERE id=? AND restored=0 AND state IN ('queued','running'))`, entry.Name()).Scan(&active); err != nil {
				return count, err
			}
			if active {
				continue
			}
			dir, err := openCleanupDirectory(filepath.Join(root, entry.Name()))
			if err != nil {
				continue
			}
			s.orphanFiles = dir
			s.orphanTaskID = entry.Name()
			continue
		}
		entries, err := s.orphanFiles.ReadDir(1)
		if errors.Is(err, io.EOF) {
			dir := s.orphanFiles.Name()
			s.orphanFiles.Close()
			s.orphanFiles = nil
			s.orphanTaskID = ""
			_ = os.Remove(dir)
			continue
		}
		if err != nil {
			s.orphanFiles.Close()
			s.orphanFiles = nil
			return count, err
		}
		entry := entries[0]
		if !ownArchiveEntry(entry.Name(), s.orphanTaskID, true) || !entry.Type().IsRegular() {
			continue
		}
		var ownerState string
		err = s.db.QueryRowContext(ctx, `SELECT state FROM backup_tasks WHERE id=? AND restored=0`, s.orphanTaskID).Scan(&ownerState)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return count, err
		}
		isTemporary := strings.HasPrefix(entry.Name(), ".") && strings.HasSuffix(entry.Name(), ".tmp")
		if ownerState == "queued" || ownerState == "running" || ownerState != "" && !isTemporary {
			continue
		}
		path := filepath.Join(s.orphanFiles.Name(), entry.Name())
		file, err := securefs.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			continue
		}
		info, err := file.Stat()
		file.Close()
		if err != nil || info.Mode().Perm() != 0600 {
			continue
		}
		if err = os.Remove(path); err == nil || os.IsNotExist(err) {
			count++
		}
	}
	return count, nil
}

// retainedArchiveNames 只保留最新的 retain 个文件名，即使目标位置累积了
// 大量旧归档，也能限制内存占用。
type retainedArchiveNames []string

func (h retainedArchiveNames) Len() int           { return len(h) }
func (h retainedArchiveNames) Less(i, j int) bool { return h[i] < h[j] }
func (h retainedArchiveNames) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *retainedArchiveNames) Push(value any)    { *h = append(*h, value.(string)) }
func (h *retainedArchiveNames) Pop() any {
	old := *h
	value := old[len(old)-1]
	*h = old[:len(old)-1]
	return value
}

func archiveEntryID(name string) string {
	if len(name) < len(".tar.gz.age")+36 {
		return ""
	}
	stem := strings.TrimSuffix(name, ".tar.gz.age")
	if len(stem) < 36 {
		return ""
	}
	return stem[len(stem)-36:]
}

func (s *Service) PruneLocal(ctx context.Context, path, planID string, retain int) error {
	if !filepath.IsAbs(path) || !uuid.ValidUUID(planID) || retain < 1 || retain > 1000 {
		return &Error{Code: "retention_cleanup_failed"}
	}
	if err := securefs.ValidateDirectory(path); err != nil {
		return &Error{Code: "retention_cleanup_failed"}
	}
	dir, err := openCleanupDirectory(filepath.Join(path, "orbitbeacon-"+planID))
	if err != nil {
		return &Error{Code: "retention_cleanup_failed"}
	}
	defer dir.Close()
	names := make(retainedArchiveNames, 0, retain)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := dir.ReadDir(100)
		if err != nil && !errors.Is(err, io.EOF) {
			return &Error{Code: "retention_cleanup_failed"}
		}
		for _, entry := range entries {
			id := archiveEntryID(entry.Name())
			if !entry.Type().IsRegular() || !validArtifactName(entry.Name(), id) {
				continue
			}
			file, err := securefs.OpenFile(filepath.Join(dir.Name(), entry.Name()), os.O_RDONLY, 0)
			if err != nil {
				return &Error{Code: "retention_cleanup_failed"}
			}
			info, err := file.Stat()
			file.Close()
			if err != nil || info.Mode().Perm() != 0600 {
				return &Error{Code: "retention_cleanup_failed"}
			}
			if len(names) < retain {
				heap.Push(&names, entry.Name())
			} else if entry.Name() > names[0] {
				names[0] = entry.Name()
				heap.Fix(&names, 0)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if len(names) < retain {
		return nil
	}
	cutoff := names[0]
	if _, err := dir.Seek(0, io.SeekStart); err != nil {
		return &Error{Code: "retention_cleanup_failed"}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := dir.ReadDir(100)
		if err != nil && !errors.Is(err, io.EOF) {
			return &Error{Code: "retention_cleanup_failed"}
		}
		for _, entry := range entries {
			if entry.Name() >= cutoff || !entry.Type().IsRegular() || !validArtifactName(entry.Name(), archiveEntryID(entry.Name())) {
				continue
			}
			file, err := securefs.OpenFile(filepath.Join(dir.Name(), entry.Name()), os.O_RDONLY, 0)
			if err != nil {
				return &Error{Code: "retention_cleanup_failed"}
			}
			info, err := file.Stat()
			file.Close()
			if err != nil || info.Mode().Perm() != 0600 {
				return &Error{Code: "retention_cleanup_failed"}
			}
			if err := os.Remove(filepath.Join(dir.Name(), entry.Name())); err != nil && !os.IsNotExist(err) {
				return &Error{Code: "retention_cleanup_failed"}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if err := dir.Sync(); err != nil {
		return &Error{Code: "retention_cleanup_failed"}
	}
	return nil
}

// CleanupLocalTemps 只访问已记录的、不活跃的、未恢复的本地任务目标位置。
// 绝不递归遍历用户路径，也不触碰已保存的包。
func (s *Service) CleanupLocalTemps(ctx context.Context, limit int) (int64, error) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT id,plan_id,destination_json,artifact_name FROM backup_tasks WHERE id>? AND kind='backup_local' AND restored=0 AND state IN ('succeeded','failed') AND plan_id IS NOT NULL ORDER BY id LIMIT ?`, s.localTempAfter, cleanupLimit(limit))
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id, planID, destination string
		name                    sql.NullString
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.planID, &c.destination, &c.name); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	var count int64
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		s.localTempAfter = c.id
		if !uuid.ValidUUID(c.id) || !uuid.ValidUUID(c.planID) || !c.name.Valid || !validArtifactName(c.name.String, c.id) {
			continue
		}
		var snapshot struct {
			Target struct {
				Path string `json:"path"`
			} `json:"target"`
		}
		if json.Unmarshal([]byte(c.destination), &snapshot) != nil || !filepath.IsAbs(snapshot.Target.Path) {
			continue
		}
		if securefs.ValidateDirectory(snapshot.Target.Path) != nil {
			continue
		}
		dir := filepath.Join(snapshot.Target.Path, "orbitbeacon-"+c.planID)
		if securefs.ValidateDirectory(dir) != nil {
			continue
		}
		var active bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM backup_tasks WHERE id=? AND state IN ('queued','running'))`, c.id).Scan(&active); err != nil {
			return count, err
		}
		if active {
			continue
		}
		path := filepath.Join(dir, "."+c.name.String+".tmp")
		file, err := securefs.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			continue
		}
		info, err := file.Stat()
		file.Close()
		if err != nil || info.Mode().Perm() != 0600 {
			continue
		}
		if err := os.Remove(path); err == nil || os.IsNotExist(err) {
			count++
		}
	}
	if len(candidates) < cleanupLimit(limit) {
		s.localTempAfter = ""
	}
	return count, nil
}
