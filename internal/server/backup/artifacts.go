package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/uuid"
)

func validArtifactName(name, id string) bool {
	if !uuid.ValidUUID(id) {
		return false
	}
	stem, ok := strings.CutPrefix(name, "orbitbeacon-")
	if !ok {
		return false
	}
	stem, ok = strings.CutSuffix(stem, ".tar.gz.age")
	if !ok || len(stem) != 16+1+len(id) || stem[16] != '-' || stem[17:] != id {
		return false
	}
	at, err := time.Parse("20060102T150405Z", stem[:16])
	return err == nil && at.Format("20060102T150405Z") == stem[:16]
}

func artifactEligible(task Task, now time.Time) error {
	if task.Restored || task.Kind != "backup_export" {
		return &Error{Code: "artifact_missing"}
	}
	if task.State != "succeeded" {
		return &Error{Code: "artifact_not_ready"}
	}
	if task.ArtifactExpiresAt == nil {
		return &Error{Code: "artifact_not_ready"}
	}
	if !now.Before(*task.ArtifactExpiresAt) {
		return &Error{Code: "artifact_expired"}
	}
	if task.artifactDeletedAt != nil || task.ArtifactName == nil || !validArtifactName(*task.ArtifactName, task.ID) {
		return &Error{Code: "artifact_missing"}
	}
	return nil
}

func (s *Service) exportDirectory(id string) (string, error) {
	if !uuid.ValidUUID(id) {
		return "", &Error{Code: "artifact_missing"}
	}
	dir := filepath.Join(s.opts.DataDir, "exports", id)
	for _, path := range []string{s.opts.DataDir, filepath.Dir(dir), dir} {
		if err := securefs.ValidateDirectory(path); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func (s *Service) openTaskArtifact(task Task) (*os.File, error) {
	if task.ArtifactName == nil || !validArtifactName(*task.ArtifactName, task.ID) {
		return nil, &Error{Code: "artifact_missing"}
	}
	dir, err := s.exportDirectory(task.ID)
	if err != nil {
		return nil, err
	}
	file, err := securefs.OpenFile(filepath.Join(dir, *task.ArtifactName), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && info.Mode().Perm() != 0600 {
		err = &Error{Code: "artifact_missing"}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// OpenArtifact exposes a seekable descriptor, not a buffered archive. The stored
// result_location is display-only and never participates in file selection.
func (s *Service) OpenArtifact(ctx context.Context, id string) (*os.File, Task, error) {
	if !uuid.ValidUUID(id) {
		return nil, Task{}, &Error{Code: "task_not_found"}
	}
	task, err := s.loadTask(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Task{}, &Error{Code: "task_not_found"}
	}
	if err != nil {
		return nil, Task{}, err
	}
	now := s.opts.Now()
	if !taskVisible(task, now) {
		return nil, Task{}, &Error{Code: "task_not_found"}
	}
	if err := artifactEligible(task, now); err != nil {
		return nil, task, err
	}
	file, err := s.openTaskArtifact(task)
	if err != nil {
		return nil, task, &Error{Code: "artifact_missing"}
	}
	task.DownloadAvailable = true
	task.PasswordAvailable = passwordEligible(task) && now.Before(*task.PasswordExpiresAt)
	return file, task, nil
}
