package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
)

// backupsRoute is reached only after route existence, method, session and CSRF
// checks. All console accounts have the same access to backup operations.
func (h *handler) backupsRoute(w http.ResponseWriter, r *http.Request, requestID string, user auth.User) bool {
	p := r.URL.Path
	if p != "/api/v1/backups/exports" && p != "/api/v1/backup-plans" && !strings.HasPrefix(p, "/api/v1/backup-plans/") && p != "/api/v1/tasks" && !strings.HasPrefix(p, "/api/v1/tasks/") {
		return false
	}
	list := r.Method == http.MethodGet && (p == "/api/v1/tasks" || p == "/api/v1/backup-plans")
	limit, before, err := backupQuery(r, list)
	if err != nil {
		h.backupError(w, err, requestID)
		return true
	}
	if h.backups == nil {
		h.backupError(w, errors.New("backup service is unavailable"), requestID)
		return true
	}
	switch p {
	case "/api/v1/backups/exports":
		var input struct {
			Categories []backup.Category `json:"categories"`
		}
		if !decode(w, r, &input, requestID) {
			return true
		}
		if _, err := backup.ValidateCategories(input.Categories); err != nil {
			h.backupError(w, &nodes.ValidationError{Fields: map[string]string{"categories": "Select one or more distinct data categories."}}, requestID)
			return true
		}
		task, err := h.backups.CreateExport(r.Context(), user.ID, input.Categories)
		if err != nil {
			h.backupError(w, err, requestID)
			return true
		}
		w.Header().Set("Location", "/api/v1/tasks/"+task.ID)
		writeJSON(w, http.StatusAccepted, task)
	case "/api/v1/backup-plans":
		if r.Method == http.MethodGet {
			page, err := h.backups.Plans(r.Context(), limit, before)
			if err != nil {
				h.backupError(w, err, requestID)
				return true
			}
			writeJSON(w, http.StatusOK, page)
			return true
		}
		h.backupPlanSave(w, r, requestID, "")
	case "/api/v1/backup-plans/preview":
		var input struct {
			Cron string `json:"cron"`
		}
		if !decode(w, r, &input, requestID) {
			return true
		}
		preview, err := h.backups.PreviewCron(input.Cron, h.now())
		if err != nil {
			h.backupError(w, err, requestID)
			return true
		}
		writeJSON(w, http.StatusOK, preview)
	case "/api/v1/tasks":
		page, err := h.backups.Tasks(r.Context(), limit, before)
		if err != nil {
			h.backupError(w, err, requestID)
			return true
		}
		writeJSON(w, http.StatusOK, page)
	default:
		if strings.HasPrefix(p, "/api/v1/backup-plans/") {
			id := strings.TrimPrefix(p, "/api/v1/backup-plans/")
			switch r.Method {
			case http.MethodPatch:
				h.backupPlanSave(w, r, requestID, id)
			case http.MethodDelete:
				if err := h.backups.DeletePlan(r.Context(), id); err != nil {
					h.backupError(w, err, requestID)
					return true
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				plan, err := h.backups.Plan(r.Context(), id)
				if err != nil {
					h.backupError(w, err, requestID)
					return true
				}
				writeJSON(w, http.StatusOK, plan)
			}
			return true
		}
		parts := strings.Split(strings.TrimPrefix(p, "/api/v1/tasks/"), "/")
		id := parts[0]
		if len(parts) == 2 {
			if parts[1] == "download" {
				h.backupDownload(w, r, requestID, id)
			} else {
				// No plaintext is fetched by task/list GETs or ordinary page loads.
				w.Header().Set("Cache-Control", "no-store")
				result, err := h.backups.Password(r.Context(), id)
				if err != nil {
					h.backupError(w, err, requestID)
					return true
				}
				writeJSON(w, http.StatusOK, result)
			}
			return true
		}
		task, err := h.backups.Task(r.Context(), id)
		if err != nil {
			h.backupError(w, err, requestID)
			return true
		}
		writeJSON(w, http.StatusOK, task)
	}
	return true
}

// Only list endpoints accept a cursor and limit. ParseQuery errors are not
// discarded: malformed encodings must not silently remove an unwanted field.
func backupQuery(r *http.Request, list bool) (int, string, error) {
	invalid := &backup.Error{Code: "invalid_input"}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, "", invalid
	}
	limit, before := 50, ""
	for key, items := range values {
		if !list || len(items) != 1 || items[0] == "" {
			return 0, "", invalid
		}
		switch key {
		case "limit":
			limit, err = strconv.Atoi(items[0])
			if err != nil || limit < 1 || limit > 100 {
				return 0, "", invalid
			}
		case "before":
			before = items[0]
			if _, _, err := management.DecodeEventCursor(before); err != nil {
				return 0, "", invalid
			}
		default:
			return 0, "", invalid
		}
	}
	return limit, before, nil
}

// PlanPatch's pointers distinguish omission but not explicit JSON null. Keep
// null rejection at this HTTP boundary without changing the service patch API.
type backupPlanInput struct{ patch backup.PlanPatch }

func (input *backupPlanInput) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return &nodes.ValidationError{Fields: map[string]string{key: "Must not be null; omit this field to retain its value."}}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&input.patch)
}

func (h *handler) backupPlanSave(w http.ResponseWriter, r *http.Request, requestID, id string) {
	var input backupPlanInput
	if !decode(w, r, &input, requestID) {
		return
	}
	plan, err := h.backups.SavePlan(r.Context(), id, input.patch)
	if err != nil {
		h.backupError(w, err, requestID)
		return
	}
	status := http.StatusOK
	if id == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, plan)
}

type backupArtifactReader struct {
	*os.File
	ctx context.Context
}

func (reader backupArtifactReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.File.Read(buffer)
}

func (h *handler) backupDownload(w http.ResponseWriter, r *http.Request, requestID, id string) {
	file, task, err := h.backups.OpenArtifact(r.Context(), id)
	if err != nil {
		h.backupError(w, err, requestID)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		h.backupError(w, &backup.Error{Code: "artifact_missing"}, requestID)
		return
	}
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Minute)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		h.backupError(w, errors.New("backup download deadline could not be set"), requestID)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": *task.ArtifactName}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, *task.ArtifactName, info.ModTime(), backupArtifactReader{File: file, ctx: r.Context()})
}

func (h *handler) backupError(w http.ResponseWriter, err error, requestID string) {
	var validation *nodes.ValidationError
	if errors.As(err, &validation) {
		writeJSON(w, http.StatusBadRequest, apiError{Code: "validation_failed", Message: validation.Error(), RequestID: requestID, Fields: validation.Fields})
		return
	}
	var coded *backup.Error
	if errors.As(err, &coded) {
		status := http.StatusInternalServerError
		message := "The backup operation could not be completed"
		switch coded.Code {
		case "invalid_input", "invalid_actor":
			status, message = http.StatusBadRequest, "The request contains invalid parameters"
		case "forbidden":
			status, message = http.StatusForbidden, "Administrator access is required"
		case "backup_queue_full":
			status, message = http.StatusTooManyRequests, "The backup queue is full; try again after a task finishes"
		case "backup_plan_busy":
			status, message = http.StatusConflict, "This plan has a queued or running backup; try again after it finishes"
		case "backup_plan_not_found", "task_not_found", "password_unavailable":
			status, message = http.StatusNotFound, "The requested backup resource is unavailable"
		case "artifact_not_ready":
			status, message = http.StatusConflict, "The download is not ready"
		case "artifact_expired", "artifact_missing", "password_expired":
			status, message = http.StatusGone, "The requested backup artifact or password is no longer available"
		case "secret_unreadable":
			status, message = http.StatusConflict, "The backup secret cannot be read with the server key"
		}
		if status != http.StatusInternalServerError {
			fail(w, status, coded.Code, message, requestID)
			return
		}
	}
	// Raw SQLite/filesystem/SDK errors may include private paths or credentials.
	// Log only a fixed diagnostic, never the wrapped error supplied by a service.
	h.logInternalError(errors.New("backup operation failed"), requestID)
	fail(w, http.StatusInternalServerError, "internal_error", "The backup operation could not be completed", requestID)
}

var _ io.ReadSeeker = backupArtifactReader{}
