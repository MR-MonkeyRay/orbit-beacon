package backup

import (
	"context"
	"errors"
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
type restoreRemote struct {
	Target          restoreEndpoint   `json:"target"`
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
	journal := restoreJournal{Version: 2, ID: id.String(), State: "prepared", Remote: &restoreRemote{Target: restoreEndpointFor(cfg), MainInstance: mainInstance, HistoryInstance: historyInstance, RollbackID: rollbackID.String()}}
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
	if restoreEndpointFor(cfg) != journal.Remote.Target {
		return errors.New("restore target configuration changed")
	}
	domains, err := storage.OpenDatabases(ctx, cfg.Database, cfg.DataDir)
	if err != nil {
		return err
	}
	defer domains.Close()
	if err = applyRemoteRestore(ctx, cfg, domains, journal, journal.State == "committed", ops); err != nil {
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

func applyRemoteRestore(ctx context.Context, cfg config.Server, domains *storage.Databases, journal restoreJournal, install bool, ops fsOps) error {
	remote := journal.Remote
	if restoreEndpointFor(cfg) != remote.Target {
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
	group, operation, files := "old", remote.RollbackID, journal.Files
	if install {
		group, operation, files = "new", journal.ID, remote.NewFiles
	}
	dir := filepath.Join(cfg.DataDir, ".restore-"+journal.ID, group)
	// 在改动任一域前检查整个镜像组；回滚再次中断也保留不可变原件。
	if err = verifyRestoreGroup(dir, files); err != nil {
		return err
	}
	for i, database := range []*storage.Database{domains.Main, domains.History} {
		if err = storage.ReplaceDomain(ctx, database, filepath.Join(dir, restoreInstallFiles[i]), operation); err != nil {
			return err
		}
		label := "install:"
		if !install {
			label = "rollback:"
		}
		if err = ops.journal(label + []string{"main", "history"}[i] + ":committed"); err != nil {
			return err
		}
	}
	key := files[2]
	destination := filepath.Join(cfg.DataDir, "secrets.key")
	if !key.Exists {
		if err = ops.remove("key:remove", destination); err != nil {
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
	if err = ops.dirSync("key:dir-sync", cfg.DataDir); err != nil {
		return err
	}
	if install {
		return ops.journal("install:key:durable")
	}
	return ops.journal("rollback:key:durable")
}
