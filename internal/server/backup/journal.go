package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/securefs"
)

const restoreJournalName = "restore-journal.json"

var restoreInstallFiles = [...]string{"orbitbeacon.db", "metrics.db", "secrets.key"}
var restoreFiles = [...]string{
	"orbitbeacon.db", "orbitbeacon.db-wal", "orbitbeacon.db-shm", "orbitbeacon.db-journal",
	"metrics.db", "metrics.db-wal", "metrics.db-shm", "metrics.db-journal", "secrets.key",
}

type restoreOriginal struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type restoreJournal struct {
	Version int               `json:"version"`
	ID      string            `json:"id"`
	State   string            `json:"state"`
	Files   []restoreOriginal `json:"files"`
}

// fsOps 刻意限定在本协议的持久化边界内。标签只有固定的协议阶段和固定的托管基名，
// 绝不含输入路径。每次调用都可在对应的 OS 操作前注入；journal 标记持久化阶段。
// 按值传递，因此并发测试无法改变生产行为。
type fsOps struct {
	copy     func(label, source, destination string) error
	fileSync func(label string, file *os.File) error
	dirSync  func(label, path string) error
	rename   func(label, source, destination string) error
	remove   func(label, path string) error
	journal  func(label string) error
}

func osFSOps() fsOps {
	return fsOps{
		copy: func(_ string, source, destination string) error {
			input, err := securefs.OpenFile(source, os.O_RDONLY, 0)
			if err != nil {
				return err
			}
			defer input.Close()
			output, err := securefs.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(output, input)
			closeErr := output.Close()
			return errors.Join(copyErr, closeErr)
		},
		fileSync: func(_ string, file *os.File) error { return file.Sync() },
		dirSync: func(_ string, path string) error {
			if err := securefs.ValidateDirectory(path); err != nil {
				return err
			}
			return securefs.SyncDirectory(path)
		},
		rename: func(_ string, source, destination string) error { return os.Rename(source, destination) },
		remove: func(_ string, path string) error {
			err := os.Remove(path)
			if os.IsNotExist(err) {
				return nil
			}
			return err
		},
		journal: func(string) error { return nil },
	}
}

// CommitRestore 安装一组已关闭、已 checkpoint 的 main/metrics/key 文件。
// 调用方在整个调用和任何恢复期间都持有 dataDir/server.lock。
// 返回的目录包含未触碰的原始文件，含 SQLite 附属文件。
func CommitRestore(ctx context.Context, dataDir, newDir string) (string, error) {
	return commitRestore(ctx, dataDir, newDir, osFSOps())
}

func commitRestore(ctx context.Context, dataDir, newDir string, ops fsOps) (rollback string, result error) {
	// 即便在 prepared 之后被取消，也必须在没有该 context 的情况下完成恢复。
	// 已提交但发布失败的情况，通过读取原子 journal 来解决，
	// 而不是猜测 rename 或 fsync 是否已生效。
	publicationAttempted := false
	defer func() {
		if result == nil {
			return
		}
		if publicationAttempted {
			if recoveryErr := recoverRestore(dataDir, ops); recoveryErr != nil {
				result = errors.Join(result, recoveryErr)
			}
		}
		result = &Error{Code: "restore_commit_failed", Err: result}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := securefs.ValidateDirectory(dataDir); err != nil {
		return "", err
	}
	if err := datadir.ValidateRestore(dataDir); err != nil {
		return "", err
	}
	if exists, err := securefs.ValidateFile(filepath.Join(dataDir, restoreJournalName)); err != nil {
		return "", err
	} else if exists {
		return "", errors.New("unrecovered restore journal")
	}
	if err := securefs.ValidateDirectory(newDir); err != nil {
		return "", err
	}
	for _, name := range restoreInstallFiles {
		if exists, err := securefs.ValidateFile(filepath.Join(newDir, name)); err != nil {
			return "", err
		} else if !exists {
			return "", errors.New("missing staged file")
		}
	}
	for _, name := range restoreFiles {
		if isRestoreInstallFile(name) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(newDir, name)); !os.IsNotExist(err) {
			return "", errors.New("staged SQLite sidecar")
		}
	}
	key, err := datadir.LoadKey(filepath.Join(newDir, "secrets.key"))
	if err != nil {
		return "", err
	}
	clear(key)
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	work := filepath.Join(dataDir, ".restore-"+id.String())
	oldDir, installDir := filepath.Join(work, "old"), filepath.Join(work, "new")
	for _, path := range []string{work, oldDir, installDir} {
		if err := os.Mkdir(path, 0700); err != nil {
			return "", err
		}
		if err := securefs.ValidateDirectory(path); err != nil {
			return "", err
		}
	}
	journal := restoreJournal{Version: 1, ID: id.String(), State: "prepared", Files: make([]restoreOriginal, 0, len(restoreFiles))}
	for _, name := range restoreFiles {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		source := filepath.Join(dataDir, name)
		exists, err := securefs.ValidateFile(source)
		if err != nil {
			return "", err
		}
		original := restoreOriginal{Name: name, Exists: exists}
		if exists {
			destination := filepath.Join(oldDir, name)
			if err := copyJournalFile(ops, "old:"+name, source, destination); err != nil {
				return "", err
			}
			original.Size, original.SHA256, err = restoreFileDigest(destination)
			if err != nil {
				return "", err
			}
		}
		journal.Files = append(journal.Files, original)
	}
	if err := ops.dirSync("old:dir-sync", oldDir); err != nil {
		return "", err
	}
	for _, name := range restoreInstallFiles {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := copyJournalFile(ops, "new:"+name, filepath.Join(newDir, name), filepath.Join(installDir, name)); err != nil {
			return "", err
		}
	}
	for _, stage := range []struct{ label, path string }{{"new:dir-sync", installDir}, {"work:dir-sync", work}, {"before-prepared:data-sync", dataDir}} {
		if err := ops.dirSync(stage.label, stage.path); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	publicationAttempted = true
	if err := writeRestoreJournal(ctx, dataDir, journal, ops); err != nil {
		return "", err
	}
	if err := ops.journal("prepared:durable"); err != nil {
		return "", err
	}
	for _, name := range restoreFiles {
		if isRestoreInstallFile(name) {
			continue
		}
		if err := ops.remove("install:"+name+":remove", filepath.Join(dataDir, name)); err != nil {
			return "", err
		}
	}
	for _, name := range restoreInstallFiles {
		if err := ops.rename("install:"+name+":rename", filepath.Join(installDir, name), filepath.Join(dataDir, name)); err != nil {
			return "", err
		}
	}
	if err := ops.dirSync("install:new-sync", installDir); err != nil {
		return "", err
	}
	if err := ops.dirSync("install:data-sync", dataDir); err != nil {
		return "", err
	}
	if err := datadir.Prepare(dataDir); err != nil {
		return "", err
	}
	if err := ops.journal("install:validated"); err != nil {
		return "", err
	}
	journal.State = "committed"
	if err := writeRestoreJournal(context.Background(), dataDir, journal, ops); err != nil {
		return "", err
	}
	if err := ops.journal("committed:durable"); err != nil {
		return "", err
	}
	if err := finishRestoreJournal(dataDir, "finish", ops); err != nil {
		return "", err
	}
	return oldDir, nil
}

func isRestoreInstallFile(name string) bool {
	for _, installed := range restoreInstallFiles {
		if name == installed {
			return true
		}
	}
	return false
}

func copyJournalFile(ops fsOps, label, source, destination string) error {
	if err := ops.copy(label+":copy", source, destination); err != nil {
		return err
	}
	file, err := securefs.OpenFile(destination, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := ops.fileSync(label+":file-sync", file)
	return errors.Join(syncErr, file.Close())
}

func restoreFileDigest(path string) (int64, string, error) {
	file, err := securefs.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return 0, "", errors.Join(err, closeErr)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func writeRestoreJournal(ctx context.Context, dataDir string, journal restoreJournal, ops fsOps) error {
	if err := ops.journal(journal.State + ":begin"); err != nil {
		return err
	}
	body, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dataDir, ".restore-journal-*.tmp")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if n, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	} else if n != len(body) {
		_ = file.Close()
		return io.ErrShortWrite
	}
	syncErr := ops.fileSync(journal.State+":file-sync", file)
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return errors.Join(syncErr, closeErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ops.rename(journal.State+":rename", path, filepath.Join(dataDir, restoreJournalName)); err != nil {
		return err
	}
	return ops.dirSync(journal.State+":data-sync", dataDir)
}

// RecoverRestore 将已准备好的事务收敛为完整的原始文件组，
// 或保留已提交的组。它必须在持久锁下、Prepare 或任何 SQLite 打开之前运行，
// 且绝不忽略 metrics 的失败。
func RecoverRestore(dataDir string) error { return recoverRestore(dataDir, osFSOps()) }

func recoverRestore(dataDir string, ops fsOps) (result error) {
	defer func() {
		if result != nil {
			result = &Error{Code: "restore_recovery_failed", Err: result}
		}
	}()
	if err := securefs.ValidateDirectory(dataDir); err != nil {
		return err
	}
	path := filepath.Join(dataDir, restoreJournalName)
	exists, err := securefs.ValidateFile(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	journal, err := readRestoreJournal(path)
	if err != nil {
		return err
	}
	if journal.State == "committed" {
		return finishRestoreJournal(dataDir, "recover-committed", ops)
	}
	oldDir := filepath.Join(dataDir, ".restore-"+journal.ID, "old")
	if err := securefs.ValidateDirectory(filepath.Dir(oldDir)); err != nil {
		return err
	}
	if err := securefs.ValidateDirectory(oldDir); err != nil {
		return err
	}
	// 在触碰任何一个活动文件之前，先检查完整的回滚组。
	// 原始文件缺失绝不授权部分修复、journal 删除或启动。
	for _, original := range journal.Files {
		path := filepath.Join(oldDir, original.Name)
		if !original.Exists {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				return errors.New("unexpected rollback file")
			}
			continue
		}
		size, sum, err := restoreFileDigest(path)
		if err != nil {
			return err
		}
		if size != original.Size || sum != original.SHA256 {
			return errors.New("rollback file digest mismatch")
		}
	}
	if err := datadir.ValidateRestore(dataDir); err != nil {
		return err
	}
	if err := ops.journal("recover:verified"); err != nil {
		return err
	}
	for _, original := range journal.Files {
		destination := filepath.Join(dataDir, original.Name)
		label := "recover:" + original.Name
		if !original.Exists {
			if err := ops.remove(label+":remove", destination); err != nil {
				return err
			}
			continue
		}
		// 在反复中断的恢复过程中保持原始文件不可变。新建一个数据目录临时文件，
		// 复制、同步、关闭后再重命名。
		id, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		temporary := filepath.Join(dataDir, ".restore-copy-"+id.String()+".tmp")
		if err := copyJournalFile(ops, label, filepath.Join(oldDir, original.Name), temporary); err != nil {
			_ = os.Remove(temporary)
			return err
		}
		if err := ops.rename(label+":rename", temporary, destination); err != nil {
			_ = os.Remove(temporary)
			return err
		}
	}
	if err := ops.dirSync("recover:restored-data-sync", dataDir); err != nil {
		return err
	}
	if err := ops.journal("recover:restored"); err != nil {
		return err
	}
	return finishRestoreJournal(dataDir, "recover-finish", ops)
}

func finishRestoreJournal(dataDir, label string, ops fsOps) error {
	if err := ops.remove(label+":remove", filepath.Join(dataDir, restoreJournalName)); err != nil {
		return err
	}
	if err := ops.dirSync(label+":data-sync", dataDir); err != nil {
		return err
	}
	return ops.journal(label + ":removed")
}

func readRestoreJournal(path string) (restoreJournal, error) {
	var journal restoreJournal
	file, err := securefs.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return journal, err
	}
	body, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return journal, errors.Join(err, closeErr)
	}
	if len(body) > 64*1024 {
		return journal, errors.New("oversized restore journal")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := checkJSONValue(decoder); err != nil {
		return journal, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return journal, errors.New("trailing restore journal data")
	}
	fields, err := requiredJSONFields(body, "version", "id", "state", "files")
	if err != nil {
		return journal, err
	}
	decoder = json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return journal, err
	}
	if !canonicalUUID(journal.ID) || journal.Version != 1 || (journal.State != "prepared" && journal.State != "committed") {
		return journal, errors.New("invalid restore journal header")
	}
	var originals []json.RawMessage
	if err := json.Unmarshal(fields["files"], &originals); err != nil {
		return journal, err
	}
	if len(originals) != len(restoreFiles) || len(journal.Files) != len(restoreFiles) {
		return journal, errors.New("invalid restore journal file count")
	}
	for index, original := range journal.Files {
		if _, err := requiredJSONFields(originals[index], "name", "exists", "size", "sha256"); err != nil {
			return journal, err
		}
		if original.Name != restoreFiles[index] || original.Size < 0 {
			return journal, errors.New("invalid restore journal file")
		}
		if !original.Exists {
			if original.Size != 0 || original.SHA256 != "" {
				return journal, errors.New("invalid absent restore file")
			}
		} else {
			if !validSHA256(original.SHA256) {
				return journal, errors.New("invalid restore journal digest")
			}
		}
	}
	return journal, nil
}
