package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/validation"
)

// backupsRoute 只在路由存在性、方法、会话和 CSRF 检查通过后才可达。
// 所有控制台账户对备份操作拥有相同权限。
func (h *handler) backupsRoute(w http.ResponseWriter, r *http.Request, requestID string, session auth.Session) {
	p := r.URL.Path
	list := r.Method == http.MethodGet && (p == "/api/v1/tasks" || p == "/api/v1/backup-plans")
	limit, before, err := backupQuery(r, list)
	if err != nil {
		h.backupError(w, err, requestID)
		return
	}
	if h.backups == nil {
		h.backupError(w, errors.New("backup service is unavailable"), requestID)
		return
	}
	switch p {
	case "/api/v1/backups/exports":
		var input struct {
			Categories []backup.Category `json:"categories"`
		}
		if !decode(w, r, &input, requestID) {
			return
		}
		if _, err := backup.ValidateCategories(input.Categories); err != nil {
			h.backupError(w, &validation.Error{Fields: map[string]string{"categories": "Select one or more distinct data categories."}}, requestID)
			return
		}
		task, err := h.backups.CreateExport(r.Context(), session.User.ID, input.Categories)
		if err != nil {
			h.backupError(w, err, requestID)
			return
		}
		w.Header().Set("Location", "/api/v1/tasks/"+task.ID)
		writeJSON(w, http.StatusAccepted, task)
	case "/api/v1/backup-plans":
		if r.Method == http.MethodGet {
			page, err := h.backups.Plans(r.Context(), limit, before)
			if err != nil {
				h.backupError(w, err, requestID)
				return
			}
			writeJSON(w, http.StatusOK, page)
			return
		}
		h.backupPlanSave(w, r, requestID, "")
	case "/api/v1/backup-plans/preview":
		var input struct {
			Cron string `json:"cron"`
		}
		if !decode(w, r, &input, requestID) {
			return
		}
		preview, err := h.backups.PreviewCron(input.Cron, h.now())
		if err != nil {
			h.backupError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, preview)
	case "/api/v1/tasks":
		page, err := h.backups.Tasks(r.Context(), limit, before)
		if err != nil {
			h.backupError(w, err, requestID)
			return
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
					return
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				plan, err := h.backups.Plan(r.Context(), id)
				if err != nil {
					h.backupError(w, err, requestID)
					return
				}
				writeJSON(w, http.StatusOK, plan)
			}
			return
		}
		parts := strings.Split(strings.TrimPrefix(p, "/api/v1/tasks/"), "/")
		id := parts[0]
		if len(parts) == 2 {
			if parts[1] == "download" {
				h.backupDownload(w, r, requestID, id)
			} else {
				// 任务/列表的 GET 或普通页面加载不会获取明文。
				w.Header().Set("Cache-Control", "no-store")
				result, err := h.backups.Password(r.Context(), id)
				if err != nil {
					h.backupError(w, err, requestID)
					return
				}
				writeJSON(w, http.StatusOK, result)
			}
			return
		}
		task, err := h.backups.Task(r.Context(), id)
		if err != nil {
			h.backupError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, task)
	}
}

// 仅列表端点接受 cursor 和 limit。ParseQuery 的错误不可丢弃：
// 畸形编码不得导致不允许的字段被静默忽略。
func backupQuery(r *http.Request, list bool) (int, string, error) {
	invalid := &backup.Error{Code: "invalid_input"}
	allowed := map[string]queryParameter{}
	if list {
		const suppliedOnce = "must be supplied once and nonempty"
		allowed["limit"] = queryParameter{nonempty: true, repeated: suppliedOnce}
		allowed["before"] = queryParameter{nonempty: true, repeated: suppliedOnce}
	}
	values, fields := parseQuery(r.URL.RawQuery, "invalid query string", allowed)
	limit := parseLimit(values, fields, 50, "must be an integer from 1 to 100")
	before, err := parseCursor(values, "before")
	if len(fields) != 0 || err != nil {
		return 0, "", invalid
	}
	return limit, before, nil
}

// PlanPatch 的指针能区分省略，但无法区分显式 JSON null。
// 在此 HTTP 边界拒绝 null，不改动服务层的 patch API。
type backupPlanInput struct{ patch backup.PlanPatch }

func (input *backupPlanInput) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return &validation.Error{Fields: map[string]string{key: "Must not be null; omit this field to retain its value."}}
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

var _ io.ReadSeeker = backupArtifactReader{}
