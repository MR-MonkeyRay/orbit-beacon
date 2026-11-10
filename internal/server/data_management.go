package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
)

const dataCleanupLimit = 5000
const dataCleanupBatch = 128

type dataCategory struct {
	id                            string
	domain                        storage.Domain
	table, keys, predicate, order string
}

var dataCategories = []dataCategory{
	// Keep all rollup ranges on buckets_retention(step,t), avoiding a full-table sort per batch.
	{"metric_history", storage.HistoryDomain, "buckets", "node_id,metric,step,t", "step IN (60,300,3600) AND t<?", "step,t,node_id,metric"},
	{"probe_history", storage.MainDomain, "probe_hourly", "node_id,target,hour", "hour<?", "hour,node_id,target"},
	{"availability_history", storage.MainDomain, "node_availability_daily", "node_id,day", "day<?", "day,node_id"},
	{"resolved_alerts", storage.MainDomain, "alert_events", "id", "state='resolved' AND resolved_at<?", "resolved_at,id"},
	{"notification_history", storage.MainDomain, "inbox_notifications", "id", "created_at<?", "created_at,id"},
	{"delivery_history", storage.MainDomain, "notification_deliveries", "id", "state IN ('sent','failed') AND updated_at<?", "updated_at,id"},
	{"audit_events", storage.MainDomain, "audit_events", "id", "created_at<?", "created_at,id"},
}

func validKeepDays(days int) bool {
	switch days {
	case 7, 30, 90, 180, 365:
		return true
	}
	return false
}

func (c dataCategory) cutoff(before time.Time) any {
	if c.domain == storage.HistoryDomain {
		return before.Unix()
	}
	if c.id == "availability_history" {
		return before.Format("2006-01-02")
	}
	return timekey.Format(before)
}

type dataDatabaseStats struct {
	Domain           storage.Domain `json:"domain"`
	Driver           string         `json:"driver"`
	SizeBytes        int64          `json:"size_bytes"`
	ReusableBytes    *int64         `json:"reusable_bytes"`
	CompactSupported bool           `json:"compact_supported"`
}

type dataCategoryStats struct {
	ID           string         `json:"id"`
	Domain       storage.Domain `json:"domain"`
	Rows         int64          `json:"rows"`
	EligibleRows int64          `json:"eligible_rows"`
}

type dataCleanupResult struct {
	ID          string `json:"id"`
	DeletedRows int64  `json:"deleted_rows"`
	HasMore     bool   `json:"has_more"`
}

func (h *handler) dataManagement(w http.ResponseWriter, r *http.Request, requestID string) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		h.dataManagementStats(w, r, requestID)
		return
	}
	if r.URL.Path == "/api/v1/data-management/compact" {
		h.dataManagementCompact(w, r, requestID)
		return
	}
	var input struct {
		Categories   []string `json:"categories"`
		KeepDays     int      `json:"keep_days"`
		Confirmation string   `json:"confirmation"`
		Before       string   `json:"before"`
	}
	if !decode(w, r, &input, requestID) {
		return
	}
	if !validKeepDays(input.KeepDays) || input.Confirmation != "CLEAN" || len(input.Categories) == 0 || len(input.Categories) > len(dataCategories) {
		h.serviceError(w, validation.Invalid("cleanup", "select categories, a supported keep_days value and confirm CLEAN"), requestID)
		return
	}
	before, err := time.Parse(time.RFC3339Nano, input.Before)
	if err != nil || before.IsZero() || before.After(h.now().UTC().AddDate(0, 0, -input.KeepDays)) {
		h.serviceError(w, validation.Invalid("before", "use the cutoff returned by data-management statistics"), requestID)
		return
	}
	before = before.UTC()
	selected := make([]dataCategory, 0, len(input.Categories))
	seen := make(map[string]bool, len(input.Categories))
	for _, id := range input.Categories {
		found := false
		for _, category := range dataCategories {
			if category.id == id && !seen[id] && (category.domain != storage.HistoryDomain || h.storage.History != nil) {
				selected = append(selected, category)
				found = true
				break
			}
		}
		if !found {
			h.serviceError(w, validation.Invalid("categories", "categories must be unique available category IDs"), requestID)
			return
		}
		seen[id] = true
	}
	main, err := h.storage.Main.Acquire(r.Context(), storage.Writer)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	defer main.Close()
	ctx := main.Context()
	leases := map[storage.Domain]*storage.Lease{storage.MainDomain: main}
	for _, category := range selected {
		if category.domain == storage.HistoryDomain && leases[storage.HistoryDomain] == nil {
			lease, err := h.storage.History.Acquire(ctx, storage.Writer)
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			defer lease.Close()
			leases[storage.HistoryDomain] = lease
		}
	}
	results := make([]dataCleanupResult, 0, len(selected))
	for _, category := range selected {
		result, err := h.cleanupDataCategory(ctx, main, leases[category.domain], category, before)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		results = append(results, result)
	}
	writeJSON(w, http.StatusOK, struct {
		Results []dataCleanupResult `json:"results"`
		Before  string              `json:"before"`
	}{results, before.Format(time.RFC3339Nano)})
}

func (h *handler) dataManagementStats(w http.ResponseWriter, r *http.Request, requestID string) {
	days := 30
	if values, ok := r.URL.Query()["keep_days"]; ok {
		var err error
		if len(values) != 1 {
			h.serviceError(w, validation.Invalid("keep_days", "select 7, 30, 90, 180 or 365"), requestID)
			return
		}
		days, err = strconv.Atoi(values[0])
		if err != nil || !validKeepDays(days) {
			h.serviceError(w, validation.Invalid("keep_days", "select 7, 30, 90, 180 or 365"), requestID)
			return
		}
	}
	before := h.now().UTC().AddDate(0, 0, -days)
	result := struct {
		Databases  []dataDatabaseStats `json:"databases"`
		Categories []dataCategoryStats `json:"categories"`
		Before     string              `json:"before"`
	}{Databases: []dataDatabaseStats{}, Categories: []dataCategoryStats{}, Before: before.Format(time.RFC3339Nano)}
	ctx := r.Context()
	for _, domain := range []storage.Domain{storage.MainDomain, storage.HistoryDomain} {
		db := h.storage.Main
		if domain == storage.HistoryDomain {
			db = h.storage.History
		}
		if db == nil {
			continue
		}
		lease, err := db.Acquire(ctx, storage.Reader)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		defer lease.Close()
		ctx = lease.Context()
		size, free, err := lease.DiskUsage(ctx)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		driver := "sqlite"
		if lease.Dialect() == storage.MySQL {
			driver = "mysql"
		}
		result.Databases = append(result.Databases, dataDatabaseStats{domain, driver, size, free, true})
		if err = func() error {
			tx, err := lease.BeginRead(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			for _, category := range dataCategories {
				if category.domain != domain {
					continue
				}
				stat := dataCategoryStats{ID: category.id, Domain: domain}
				if err := tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(CASE WHEN "+category.predicate+" THEN 1 ELSE 0 END),0) FROM "+category.table, category.cutoff(before)).Scan(&stat.Rows, &stat.EligibleRows); err != nil {
					return err
				}
				result.Categories = append(result.Categories, stat)
			}
			return tx.Commit()
		}(); err != nil {
			h.serviceError(w, err, requestID)
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *handler) dataAudit(ctx context.Context, main *storage.Lease, action, target string, details audit.Details) error {
	tx, err := main.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = audit.InsertDetails(ctx, tx, action, target, h.now(), details); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *handler) cleanupDataCategory(ctx context.Context, main, lease *storage.Lease, category dataCategory, before time.Time) (result dataCleanupResult, err error) {
	result.ID = category.id
	details := audit.Details{"before": before.Format(time.RFC3339Nano)}
	if category.domain == storage.HistoryDomain {
		if err = h.dataAudit(ctx, main, "data.cleanup_intent", category.id, details); err != nil {
			return
		}
	}
	ctx = lease.Context()
	keys := strings.Split(category.keys, ",")
	conditions := make([]string, len(keys))
	for i, key := range keys {
		conditions[i] = key + "=?"
	}
	for result.DeletedRows < dataCleanupLimit {
		limit := min(dataCleanupBatch, dataCleanupLimit-int(result.DeletedRows))
		var removed int64
		err = func() error {
			tx, err := lease.BeginWrite(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			removed, err = storage.DeleteBatch(ctx, tx, "SELECT "+category.keys+" FROM "+category.table+" WHERE "+category.predicate+" ORDER BY "+category.order+" LIMIT ?", "DELETE FROM "+category.table+" WHERE "+strings.Join(conditions, " AND "), category.cutoff(before), limit)
			if err != nil {
				return err
			}
			if category.domain == storage.MainDomain && removed > 0 {
				if err = audit.InsertDetails(ctx, tx, "data.cleanup", category.id, h.now(), audit.Details{"before": before.Format(time.RFC3339Nano), "deleted_rows": removed}); err != nil {
					return err
				}
			}
			return tx.Commit()
		}()
		if err != nil {
			return
		}
		result.DeletedRows += removed
		if removed < int64(limit) {
			break
		}
	}
	err = lease.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+category.table+" WHERE "+category.predicate+")", category.cutoff(before)).Scan(&result.HasMore)
	if err != nil {
		return
	}
	if category.domain == storage.HistoryDomain {
		details["deleted_rows"], details["has_more"] = result.DeletedRows, result.HasMore
		err = h.dataAudit(ctx, main, "data.cleanup_complete", category.id, details)
	}
	return
}

func (h *handler) dataManagementCompact(w http.ResponseWriter, r *http.Request, requestID string) {
	var input struct {
		Domain       storage.Domain `json:"domain"`
		Confirmation string         `json:"confirmation"`
	}
	if !decode(w, r, &input, requestID) {
		return
	}
	if (input.Domain != storage.MainDomain && input.Domain != storage.HistoryDomain) || input.Confirmation != "COMPACT" || (input.Domain == storage.HistoryDomain && h.storage.History == nil) {
		h.serviceError(w, validation.Invalid("compact", "select an available domain and confirm COMPACT"), requestID)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(31 * time.Minute)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		h.serviceError(w, err, requestID)
		return
	}
	main, err := h.storage.Main.Acquire(r.Context(), storage.Writer)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	defer main.Close()
	lease := main
	if input.Domain == storage.HistoryDomain {
		lease, err = h.storage.History.Acquire(main.Context(), storage.Writer)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		defer lease.Close()
	}
	if err = h.dataAudit(lease.Context(), main, "data.compact_intent", string(input.Domain), nil); err == nil {
		err = lease.Compact(lease.Context())
	}
	if err == nil {
		err = h.dataAudit(lease.Context(), main, "data.compact_complete", string(input.Domain), nil)
	}
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Domain storage.Domain `json:"domain"`
	}{input.Domain})
}
