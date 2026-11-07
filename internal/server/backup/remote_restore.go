package backup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/server/storage"
)

// 只持久化目标身份，不保存账户、凭据路径或 DSN。
type restoreEndpoint struct {
	Driver          string `json:"driver"`
	Network         string `json:"network"`
	Address         string `json:"address"`
	MainDatabase    string `json:"main_database"`
	HistoryDatabase string `json:"history_database"`
}

const restoreMetadataName = "storage-metadata.json"

type restoreRemote struct {
	Target          restoreEndpoint   `json:"target"`
	Standby         *restoreEndpoint  `json:"standby,omitempty"`
	Metadata        restoreOriginal   `json:"metadata"`
	MainInstance    string            `json:"main_instance"`
	HistoryInstance string            `json:"history_instance"`
	RollbackID      string            `json:"rollback_id"`
	NewFiles        []restoreOriginal `json:"new_files"`
}

func restoreEndpointFor(cfg config.Server) restoreEndpoint {
	e := cfg.Database.Primary
	if e.Driver == "" {
		e.Driver = "sqlite"
	}
	return restoreEndpoint{Driver: e.Driver, Network: e.Network, Address: e.Address, MainDatabase: e.MainDatabase, HistoryDatabase: e.HistoryDatabase}
}

func restoreStandbyFor(cfg config.Server) *restoreEndpoint {
	if cfg.Database.Standby == nil {
		return nil
	}
	standby := cfg
	standby.Database.Primary = *cfg.Database.Standby
	endpoint := restoreEndpointFor(standby)
	return &endpoint
}

func restoreTargetMatches(cfg config.Server, remote *restoreRemote) bool {
	standby := restoreStandbyFor(cfg)
	return restoreEndpointFor(cfg) == remote.Target &&
		((standby == nil && remote.Standby == nil) || (standby != nil && remote.Standby != nil && *standby == *remote.Standby))
}

func validateRestoreRemote(remote *restoreRemote) error {
	if remote == nil || !canonicalUUID(remote.RollbackID) || len(remote.NewFiles) != len(restoreInstallFiles) {
		return errors.New("invalid remote restore journal")
	}
	if remote.Target.Driver == "mysql" && (!canonicalUUID(remote.MainInstance) || !canonicalUUID(remote.HistoryInstance)) {
		return errors.New("invalid restore target identity")
	}
	if remote.Target.Driver != "mysql" && remote.Target.Driver != "sqlite" {
		return errors.New("invalid restore driver")
	}
	if remote.Standby != nil && (remote.Standby.Driver != "mysql" && remote.Standby.Driver != "sqlite" || !canonicalUUID(remote.MainInstance) || !canonicalUUID(remote.HistoryInstance)) {
		return errors.New("invalid restore standby identity")
	}
	if remote.Metadata.Name != restoreMetadataName || !remote.Metadata.Exists || remote.Metadata.Size < 1 || remote.Metadata.Size > 1024*1024 || !validSHA256(remote.Metadata.SHA256) {
		return errors.New("invalid restore metadata image")
	}
	for i, file := range remote.NewFiles {
		if file.Name != restoreInstallFiles[i] || !file.Exists || file.Size < 0 || !validSHA256(file.SHA256) {
			return errors.New("invalid staged restore file")
		}
	}
	return nil
}

func commitRemoteRestore(ctx context.Context, cfg config.Server, domains *storage.Databases, newDir string, ops fsOps) (rollback string, result error) {
	publicationAttempted := false
	defer func() {
		if result == nil {
			return
		}
		if publicationAttempted {
			recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// 重新读取持久决定，不能把失败的 committed rename 当作回滚或成功。
			path := filepath.Join(cfg.DataDir, restoreJournalName)
			if exists, e := securefs.ValidateFile(path); e != nil {
				result = errors.Join(result, e)
			} else if exists {
				journal, e := readRestoreJournal(path)
				if e == nil {
					e = recoverRemoteRestore(recoveryCtx, cfg, journal, ops)
				}
				result = errors.Join(result, e)
			}
		}
		result = &Error{Code: "restore_commit_failed", Err: result}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := securefs.ValidateDirectory(cfg.DataDir); err != nil {
		return "", err
	}
	if exists, err := securefs.ValidateFile(filepath.Join(cfg.DataDir, restoreJournalName)); err != nil {
		return "", err
	} else if exists {
		return "", errors.New("unrecovered restore journal")
	}
	if err := securefs.ValidateDirectory(newDir); err != nil {
		return "", err
	}
	for _, name := range restoreInstallFiles {
		exists, e := securefs.ValidateFile(filepath.Join(newDir, name))
		if e != nil {
			return "", e
		}
		if !exists {
			return "", errors.New("missing staged restore file")
		}
	}
	for _, name := range restoreFiles {
		if !isRestoreInstallFile(name) {
			if exists, e := securefs.ValidateFile(filepath.Join(newDir, name)); e != nil {
				return "", e
			} else if exists {
				return "", errors.New("staged SQLite sidecar")
			}
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
	rollbackID, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	mainInstance, err := domains.Main.InstanceID(ctx)
	if err != nil {
		return "", err
	}
	historyInstance, err := domains.History.InstanceID(ctx)
	if err != nil {
		return "", err
	}
	metadata, err := domains.CaptureRestoreMetadata(ctx)
	if err != nil {
		return "", err
	}
	if len(metadata) == 0 {
		metadata = json.RawMessage("null")
	}
	if !json.Valid(metadata) || len(metadata) > 1024*1024 {
		return "", errors.New("invalid restore metadata")
	}
	work := filepath.Join(cfg.DataDir, ".restore-"+id.String())
	oldDir, installDir := filepath.Join(work, "old"), filepath.Join(work, "new")
	for _, dir := range []string{work, oldDir, installDir} {
		if err = os.Mkdir(dir, 0700); err != nil {
			return "", err
		}
		if err = securefs.ValidateDirectory(dir); err != nil {
			return "", err
		}
	}
	journal := restoreJournal{Version: 2, ID: id.String(), State: "prepared", Remote: &restoreRemote{Target: restoreEndpointFor(cfg), Standby: restoreStandbyFor(cfg), MainInstance: mainInstance, HistoryInstance: historyInstance, RollbackID: rollbackID.String()}}
	for i, database := range []*storage.Database{domains.Main, domains.History} {
		name := restoreInstallFiles[i]
		snapshot, err := restoreDomainSnapshot(ctx, database, filepath.Join(oldDir, name))
		if err != nil {
			return "", err
		}
		if err = snapshot.Close(); err != nil {
			return "", err
		}
		file, err := securefs.OpenFile(filepath.Join(oldDir, name), os.O_RDWR, 0)
		if err != nil {
			return "", err
		}
		err = errors.Join(ops.fileSync("old:"+name+":file-sync", file), file.Close())
		if err != nil {
			return "", err
		}
	}
	keyPath := filepath.Join(cfg.DataDir, "secrets.key")
	keyExists, err := securefs.ValidateFile(keyPath)
	if err != nil {
		return "", err
	}
	if keyExists {
		if err = copyJournalFile(ops, "old:secrets.key", keyPath, filepath.Join(oldDir, "secrets.key")); err != nil {
			return "", err
		}
	}
	metadataPath := filepath.Join(oldDir, restoreMetadataName)
	metadataFile, err := securefs.OpenFile(metadataPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	n, writeErr := metadataFile.Write(metadata)
	if writeErr == nil && n != len(metadata) {
		writeErr = io.ErrShortWrite
	}
	err = errors.Join(writeErr, ops.fileSync("old:storage-metadata:file-sync", metadataFile), metadataFile.Close())
	if err != nil {
		return "", err
	}
	journal.Remote.Metadata = restoreOriginal{Name: restoreMetadataName, Exists: true}
	journal.Remote.Metadata.Size, journal.Remote.Metadata.SHA256, err = restoreFileDigest(metadataPath)
	if err != nil {
		return "", err
	}
	for _, name := range restoreInstallFiles {
		original := restoreOriginal{Name: name, Exists: name != "secrets.key" || keyExists}
		if original.Exists {
			original.Size, original.SHA256, err = restoreFileDigest(filepath.Join(oldDir, name))
			if err != nil {
				return "", err
			}
		}
		journal.Files = append(journal.Files, original)
		if err = copyJournalFile(ops, "new:"+name, filepath.Join(newDir, name), filepath.Join(installDir, name)); err != nil {
			return "", err
		}
		staged := restoreOriginal{Name: name, Exists: true}
		staged.Size, staged.SHA256, err = restoreFileDigest(filepath.Join(installDir, name))
		if err != nil {
			return "", err
		}
		journal.Remote.NewFiles = append(journal.Remote.NewFiles, staged)
	}
	for _, dir := range []string{oldDir, installDir, work, cfg.DataDir} {
		if err = ops.dirSync("prepare:dir-sync", dir); err != nil {
			return "", err
		}
	}
	publicationAttempted = true
	if err = writeRestoreJournal(ctx, cfg.DataDir, journal, ops); err != nil {
		return "", err
	}
	if err = ops.journal("prepared:durable"); err != nil {
		return "", err
	}
	if err = applyRemoteRestore(ctx, cfg, domains, journal, true, ops); err != nil {
		return "", err
	}
	journal.State = "committed"
	if err = writeRestoreJournal(context.Background(), cfg.DataDir, journal, ops); err != nil {
		return "", err
	}
	if err = ops.journal("committed:durable"); err != nil {
		return "", err
	}
	if err = domains.FinishRestore(ctx, journal.ID, metadata, false); err != nil {
		return "", err
	}
	if err = ops.journal("finish:storage:durable"); err != nil {
		return "", err
	}
	if err = finishRemoteRestore(cfg.DataDir, journal, "finish", ops); err != nil {
		return "", err
	}
	return oldDir, nil
}

func recoverRemoteRestore(ctx context.Context, cfg config.Server, journal restoreJournal, ops fsOps) (result error) {
	defer func() {
		if result != nil {
			result = &Error{Code: "restore_recovery_failed", Err: result}
		}
	}()
	if journal.Remote == nil {
		return errors.New("missing restore target")
	}
	if !restoreTargetMatches(cfg, journal.Remote) {
		return errors.New("restore target configuration changed")
	}
	domains, err := storage.OpenOfflineDatabases(ctx, cfg.Database, cfg.DataDir)
	if err != nil {
		return err
	}
	defer domains.Close()
	metadata, err := readRestoreMetadata(cfg.DataDir, journal)
	if err != nil {
		return err
	}
	if journal.State == "committed" {
		// committed 已证明各域和 key 完整安装；重复发布不重放逻辑行。
		if err = verifyRemoteTarget(ctx, cfg, domains, journal.Remote); err != nil {
			return err
		}
		if err = verifyRestoreGroup(filepath.Join(cfg.DataDir, ".restore-"+journal.ID, "new"), journal.Remote.NewFiles); err != nil {
			return err
		}
	} else if err = applyRemoteRestore(ctx, cfg, domains, journal, false, ops); err != nil {
		return err
	}
	if err = domains.FinishRestore(ctx, journal.ID, metadata, journal.State != "committed"); err != nil {
		return err
	}
	if err = ops.journal("finish:storage:durable"); err != nil {
		return err
	}
	return finishRemoteRestore(cfg.DataDir, journal, "recover-remote", ops)
}

func verifyRestoreGroup(dir string, files []restoreOriginal) error {
	if err := securefs.ValidateDirectory(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := securefs.ValidateDirectory(dir); err != nil {
		return err
	}
	for _, file := range files {
		path := filepath.Join(dir, file.Name)
		exists, err := securefs.ValidateFile(path)
		if err != nil {
			return err
		}
		if exists != file.Exists {
			return errors.New("restore image missing or unexpected")
		}
		if exists {
			size, sum, err := restoreFileDigest(path)
			if err != nil {
				return err
			}
			if size != file.Size || sum != file.SHA256 {
				return errors.New("restore image digest mismatch")
			}
		}
	}
	return nil
}

func readRestoreMetadata(dataDir string, journal restoreJournal) (json.RawMessage, error) {
	dir := filepath.Join(dataDir, ".restore-"+journal.ID, "old")
	if err := verifyRestoreGroup(dir, []restoreOriginal{journal.Remote.Metadata}); err != nil {
		return nil, err
	}
	file, err := securefs.OpenFile(filepath.Join(dir, restoreMetadataName), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err = errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if len(body) > 1024*1024 || !json.Valid(body) {
		return nil, errors.New("invalid restore metadata")
	}
	return json.RawMessage(body), nil
}

func finishRemoteRestore(dataDir string, journal restoreJournal, label string, ops fsOps) error {
	if err := finishRestoreJournal(dataDir, label, ops); err != nil {
		return err
	}
	work := filepath.Join(dataDir, ".restore-"+journal.ID)
	if err := os.RemoveAll(filepath.Join(work, "new")); err != nil {
		return err
	}
	return securefs.SyncDirectory(work)
}

func verifyRemoteTarget(ctx context.Context, cfg config.Server, domains *storage.Databases, remote *restoreRemote) error {
	if !restoreTargetMatches(cfg, remote) {
		return errors.New("restore target configuration changed")
	}
	mainID, err := domains.Main.InstanceID(ctx)
	if err != nil {
		return err
	}
	historyID, err := domains.History.InstanceID(ctx)
	if err != nil {
		return err
	}
	if mainID != remote.MainInstance || historyID != remote.HistoryInstance {
		return errors.New("restore target instance changed")
	}
	return nil
}

func applyRemoteRestore(ctx context.Context, cfg config.Server, domains *storage.Databases, journal restoreJournal, install bool, ops fsOps) error {
	remote := journal.Remote
	if err := verifyRemoteTarget(ctx, cfg, domains, remote); err != nil {
		return err
	}
	group, operation, files := "old", remote.RollbackID, journal.Files
	if install {
		group, operation, files = "new", journal.ID, remote.NewFiles
	}
	dir := filepath.Join(cfg.DataDir, ".restore-"+journal.ID, group)
	// 在改动任一域前检查整个镜像组；回滚再次中断也保留不可变原件。
	if err := verifyRestoreGroup(dir, files); err != nil {
		return err
	}
	if _, err := readRestoreMetadata(cfg.DataDir, journal); err != nil {
		return err
	}
	if err := domains.BeginRestore(ctx, journal.ID, []storage.Domain{storage.MainDomain, storage.HistoryDomain}); err != nil {
		return err
	}
	if err := ops.journal("restore:pending:durable"); err != nil {
		return err
	}
	for i, database := range []*storage.Database{domains.Main, domains.History} {
		if err := storage.ReplaceDomain(ctx, database, filepath.Join(dir, restoreInstallFiles[i]), operation); err != nil {
			return err
		}
		label := "install:"
		if !install {
			label = "rollback:"
		}
		if err := ops.journal(label + []string{"main", "history"}[i] + ":committed"); err != nil {
			return err
		}
	}
	key := files[2]
	destination := filepath.Join(cfg.DataDir, "secrets.key")
	if !key.Exists {
		if err := ops.remove("key:remove", destination); err != nil {
			return err
		}
	} else {
		id, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		temporary := filepath.Join(cfg.DataDir, ".restore-key-"+id.String()+".tmp")
		defer os.Remove(temporary)
		if err = copyJournalFile(ops, "key", filepath.Join(dir, "secrets.key"), temporary); err != nil {
			return err
		}
		if _, err = securefs.ValidateFile(destination); err != nil {
			return err
		}
		if err = ops.rename("key:rename", temporary, destination); err != nil {
			return err
		}
	}
	if err := ops.dirSync("key:dir-sync", cfg.DataDir); err != nil {
		return err
	}
	if install {
		return ops.journal("install:key:durable")
	}
	return ops.journal("rollback:key:durable")
}
