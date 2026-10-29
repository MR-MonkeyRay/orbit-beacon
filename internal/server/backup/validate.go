package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/robfig/cron/v3"

	"orbitbeacon/internal/identity"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/timekey"
)

// ValidatePayloadData reads only the registry's named columns. Callers validate
// the transfer schema first and retain responsibility for final-state FK checks.
// Neither validation nor the shared domain parsers perform network/filesystem I/O.
func ValidatePayloadData(ctx context.Context, db *sql.DB, category Category, key []byte) error {
	specs := tablesFor(category)
	if len(specs) == 0 {
		return invalidPayload("category", "")
	}
	users := 0
	var notificationBox, backupBox *secretbox.Box
	if category == Configuration {
		var err error
		notificationBox, err = secretbox.New(key, "orbitbeacon notification channel v1")
		if err != nil {
			return invalidPayload("key", "")
		}
		backupBox, err = secretbox.New(key, "orbitbeacon backup secrets v1")
		if err != nil {
			return invalidPayload("key", "")
		}
	}
	for _, table := range specs {
		rows, err := db.QueryContext(ctx, "SELECT "+columnNames(table)+" FROM "+table.Name)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return invalidPayload(table.Name, "")
		}
		values := make([]any, len(table.Columns))
		destinations := make([]any, len(values))
		for i := range values {
			destinations[i] = &values[i]
		}
		row := make(payloadRow, len(values))
		for rows.Next() {
			if err = rows.Scan(destinations...); err != nil {
				rows.Close()
				return invalidPayload(table.Name, "")
			}
			for i, c := range table.Columns {
				value := values[i]
				if value == nil {
					if !nullableColumn(table.Name, c.Name) {
						rows.Close()
						return invalidPayload(table.Name, "")
					}
					row[c.Name] = nil
					continue
				}
				valid := false
				switch c.Type {
				case "TEXT":
					_, valid = value.(string)
				case "INTEGER":
					_, valid = value.(int64)
				case "REAL":
					switch value.(type) {
					case float64, int64:
						valid = true
					}
				case "BLOB":
					_, valid = value.([]byte)
				}
				if !valid {
					rows.Close()
					return invalidPayload(table.Name, "")
				}
				row[c.Name] = value
				if strings.HasSuffix(c.Name, "_at") {
					if !canonicalTime(row.text(c.Name)) {
						rows.Close()
						return invalidPayload(table.Name, "")
					}
				}
			}
			if err = validatePayloadRow(table.Name, row, notificationBox, backupBox); err != nil {
				rows.Close()
				return invalidPayload(table.Name, row.text("id"))
			}
			if table.Name == "users" {
				users++
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return invalidPayload(table.Name, "")
		}
	}
	if category == Configuration && users == 0 {
		return invalidPayload("users", "")
	}
	return nil
}

func invalidPayload(table, id string) error {
	// Only fixed registry names and syntactically valid UUIDs reach diagnostics.
	if !identity.ValidUUID(id) {
		id = ""
	}
	if id != "" {
		return &Error{Code: "restore_invalid_data", Err: fmt.Errorf("%s %s", table, id)}
	}
	return &Error{Code: "restore_invalid_data", Err: fmt.Errorf("%s", table)}
}

type payloadRow map[string]any

func (r payloadRow) text(k string) string   { v, _ := r[k].(string); return v }
func (r payloadRow) integer(k string) int64 { v, _ := r[k].(int64); return v }
func (r payloadRow) number(k string) float64 {
	if v, ok := r[k].(int64); ok {
		return float64(v)
	}
	v, _ := r[k].(float64)
	return v
}
func (r payloadRow) blob(k string) []byte { v, _ := r[k].([]byte); return v }
func (r payloadRow) pointer(k string) *string {
	if r[k] == nil {
		return nil
	}
	v := r.text(k)
	return &v
}
func (r payloadRow) floatPointer(k string) *float64 {
	if r[k] == nil {
		return nil
	}
	v := r.number(k)
	return &v
}
func nullableColumn(table, column string) bool {
	switch table {
	case "nodes":
		return oneOf(column, "group_id", "token_hash", "installation_id", "first_enrolled_at")
	case "alert_rules":
		return oneOf(column, "scope_id", "comparator", "threshold")
	case "notification_channels":
		return oneOf(column, "secret_ciphertext", "url_ciphertext")
	case "backup_plans":
		return oneOf(column, "credentials_ciphertext", "retain_count")
	case "node_observations":
		return column == "last_seen"
	case "probe_latest":
		return oneOf(column, "latency_ms", "status_code")
	case "audit_events":
		return oneOf(column, "actor_id", "target_id")
	case "alert_events":
		return oneOf(column, "value", "resolved_at", "last_notified_at")
	case "backup_tasks":
		return oneOf(column, "plan_id", "actor_id", "scheduled_at", "started_at", "finished_at", "artifact_name", "artifact_bytes", "artifact_sha256", "result_location")
	}
	return false
}
func oneOf(value string, allowed ...string) bool {
	for _, v := range allowed {
		if v == value {
			return true
		}
	}
	return false
}
func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func canonicalTime(raw string) bool {
	t, err := timekey.Parse(raw)
	return err == nil && timekey.Format(t) == raw
}
func objectJSON(raw string, dst any) error {
	if !utf8.ValidString(raw) || !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return errors.New("invalid object")
	}
	return json.Unmarshal([]byte(raw), dst)
}
func jsonObject(raw string) bool {
	var obj map[string]json.RawMessage
	return objectJSON(raw, &obj) == nil && obj != nil
}
func validText(s string, max int) bool { return strings.TrimSpace(s) != "" && len(s) <= max }
func invalidRow() error                { return errors.New("invalid row") }

func validatePayloadRow(table string, r payloadRow, notificationBox, backupBox *secretbox.Box) error {
	if r["id"] != nil && !identity.ValidUUID(r.text("id")) {
		return invalidRow()
	}
	switch table {
	case "settings":
		if r.text("key") != "public_page_enabled" || !oneOf(r.text("value"), "true", "false") {
			return invalidRow()
		}
	case "users":
		if username := r.text("username"); len(username) < 1 || len(username) > 128 || !utf8.ValidString(username) {
			return invalidRow()
		}
		if flag := r.integer("must_change_password"); flag < 0 || flag > 1 {
			return invalidRow()
		}
		return auth.ValidatePasswordHash(r.text("password_hash"))
	case "groups":
		if !validText(r.text("name"), 255) {
			return invalidRow()
		}
	case "nodes":
		return validateNode(r)
	case "node_tags":
		if !identity.ValidUUID(r.text("node_id")) || !validText(r.text("tag"), 255) {
			return invalidRow()
		}
	case "alert_rules":
		if r.integer("enabled") < 0 || r.integer("enabled") > 1 {
			return invalidRow()
		}
		return alerts.ValidateRuleSyntax(alerts.Rule{Name: r.text("name"), Enabled: r.integer("enabled") == 1, ScopeKind: r.text("scope_kind"), ScopeID: r.pointer("scope_id"), Metric: r.text("metric"), Comparator: r.pointer("comparator"), Threshold: r.floatPointer("threshold"), ForSeconds: int(r.integer("for_seconds")), Severity: r.text("severity")})
	case "notification_channels":
		return validateChannel(r, notificationBox)
	case "alert_rule_channels":
		if !identity.ValidUUID(r.text("rule_id")) || !identity.ValidUUID(r.text("channel_id")) {
			return invalidRow()
		}
	case "backup_plans":
		return validatePlanRow(r, backupBox)
	case "node_observations":
		if r.text("node_id") == "" {
			return invalidRow()
		}
		var system v1.System
		if objectJSON(r.text("system"), &system) != nil {
			return invalidRow()
		}
		if err := system.Validate(); err != nil {
			return err
		}
		if r.text("agent_runtime") != "" {
			var runtime v1.RuntimeSettings
			if objectJSON(r.text("agent_runtime"), &runtime) != nil {
				return invalidRow()
			}
			return runtime.Validate()
		}
	case "traffic_hourly":
		if r.text("node_id") == "" || !canonicalTime(r.text("period_start")) || r.integer("rx_bytes") < 0 || r.integer("tx_bytes") < 0 {
			return invalidRow()
		}
		at, _ := timekey.Parse(r.text("period_start"))
		return (v1.Traffic{Epoch: r.text("epoch"), PeriodStart: at, RxBytes: v1.DecimalUint64(strconv.FormatInt(r.integer("rx_bytes"), 10)), TxBytes: v1.DecimalUint64(strconv.FormatInt(r.integer("tx_bytes"), 10))}).Validate()
	case "traffic_daily":
		day, err := time.Parse("2006-01-02", r.text("day"))
		if err != nil || day.Format("2006-01-02") != r.text("day") || r.text("node_id") == "" || r.integer("rx_bytes") < 0 || r.integer("tx_bytes") < 0 {
			return invalidRow()
		}
	case "probe_latest":
		if r.text("node_id") == "" || r.integer("ok") < 0 || r.integer("ok") > 1 {
			return invalidRow()
		}
		at, _ := timekey.Parse(r.text("checked_at"))
		var code *int
		if r["status_code"] != nil {
			v := int(r.integer("status_code"))
			code = &v
		}
		return (v1.ProbeResult{Target: r.text("target"), OK: r.integer("ok") == 1, LatencyMS: r.floatPointer("latency_ms"), Error: r.text("error"), StatusCode: code, CheckedAt: at}).Validate()
	case "probe_hourly":
		at, err := timekey.Parse(r.text("hour"))
		if err != nil || timekey.Format(at) != r.text("hour") || !at.Equal(at.Truncate(time.Hour)) || r.text("node_id") == "" || r.integer("checks") < 0 || r.integer("failures") < 0 || r.integer("failures") > r.integer("checks") || !finite(r.number("latency_sum_ms")) || r.number("latency_sum_ms") < 0 {
			return invalidRow()
		}
		return v1.ValidateProbeTarget(r.text("target"))
	case "buckets":
		step := r.integer("step")
		if r.text("node_id") == "" || r.text("metric") == "" || !oneOf(strconv.FormatInt(step, 10), "60", "300", "3600") || r.integer("t")%step != 0 || r.integer("samples") < 1 || !finite(r.number("total")) || !finite(r.number("peak")) || r.number("total") < 0 || r.number("peak") < 0 || r.number("peak") > r.number("total") {
			return invalidRow()
		}
		if !history.ValidValue(r.text("metric"), r.number("peak")) || !history.ValidValue(r.text("metric"), r.number("total")/float64(r.integer("samples"))) {
			return invalidRow()
		}
	case "audit_events":
		if !jsonObject(r.text("details")) {
			return invalidRow()
		}
	case "alert_events":
		if !oneOf(r.text("state"), "firing", "resolved") || (r.text("state") == "firing") != (r["resolved_at"] == nil) || !jsonObject(r.text("rule_snapshot")) || (r["value"] != nil && !finite(r.number("value"))) {
			return invalidRow()
		}
		// Resolved legacy events can legitimately have an empty rule snapshot.
		if r.text("state") == "firing" {
			_, err := alerts.ParseRuleSnapshot([]byte(r.text("rule_snapshot")))
			return err
		}
		var rule alerts.Rule
		return json.Unmarshal([]byte(r.text("rule_snapshot")), &rule)
	case "notification_deliveries":
		if !oneOf(r.text("state"), "sent", "failed") || r.integer("attempts") < 0 || r.integer("attempts") > 5 || len(r.text("last_error_code")) > 40 || r.text("lease_until") != "" || r.integer("coalesced_count") < 1 {
			return invalidRow()
		}
		_, err := notify.ParsePayload([]byte(r.text("payload_json")))
		return err
	case "inbox_notifications":
		if !oneOf(r.text("kind"), "task", "alert") || r.text("source_id") == "" || r.text("transition") == "" || inbox.ValidateDetails(r.text("kind"), json.RawMessage(r.text("details_json"))) != nil {
			return invalidRow()
		}
	case "inbox_reads":
		if !identity.ValidUUID(r.text("user_id")) || !identity.ValidUUID(r.text("notification_id")) {
			return invalidRow()
		}
	case "backup_tasks":
		return validateTaskRow(r)
	default:
		return invalidRow()
	}
	return nil
}

func validateNode(r payloadRow) error {
	if !validText(r.text("name"), 255) || !oneOf(r.text("status"), "disabled", "unknown", "pending") || (r["group_id"] != nil && !identity.ValidUUID(r.text("group_id"))) || r.integer("credential_epoch") < 0 {
		return invalidRow()
	}
	if err := v1.DecimalUint64(r.text("run_generation")).Validate(); err != nil {
		return err
	}
	if r["token_hash"] != nil && len(r.blob("token_hash")) != 32 {
		return invalidRow()
	}
	if len(r.text("report_session_id")) > 255 {
		return invalidRow()
	}
	if r["installation_id"] != nil && !validText(r.text("installation_id"), 255) {
		return invalidRow()
	}
	var public nodes.PublicSettings
	if objectJSON(r.text("public_settings"), &public) != nil {
		return invalidRow()
	}
	if err := public.Validate(); err != nil {
		return err
	}
	if r.integer("billing_day") < 1 || r.integer("billing_day") > 28 {
		return invalidRow()
	}
	return v1.DecimalUint64(r.text("quota_bytes")).Validate()
}

func validateChannel(r payloadRow, box *secretbox.Box) error {
	if r.integer("enabled") < 0 || r.integer("enabled") > 1 {
		return invalidRow()
	}
	var cfg notify.Config
	if objectJSON(r.text("config_json"), &cfg) != nil {
		return invalidRow()
	}
	var err error
	var secret, url []byte
	if r["secret_ciphertext"] != nil {
		secret, err = box.Open(r.text("id"), r.blob("secret_ciphertext"))
		if err != nil {
			return err
		}
		defer clear(secret)
	}
	if r["url_ciphertext"] != nil {
		url, err = box.Open(r.text("id")+":url", r.blob("url_ciphertext"))
		if err != nil {
			return err
		}
		defer clear(url)
	}
	cfg.URL = string(url)
	cfg.URLSet = false
	if r.text("kind") != "webhook" && len(url) > 0 {
		return invalidRow()
	}
	// Machine-level insecure opt-in is not configuration data. Preserve syntax
	// accepted with that opt-in; sending still enforces the target's runtime policy.
	return notify.ValidateChannel(notify.Channel{Name: r.text("name"), Kind: r.text("kind"), Config: cfg}, string(secret), true)
}

func validateTaskRow(r payloadRow) error {
	if !oneOf(r.text("kind"), "backup_export", "backup_local", "backup_remote") || !oneOf(r.text("state"), "succeeded", "failed") || !oneOf(r.text("phase"), "", "snapshot", "packaging", "storing") || r["finished_at"] == nil || r.integer("restored") < 0 || r.integer("restored") > 1 {
		return invalidRow()
	}
	if err := validateCategoriesJSON(r.text("categories_json")); err != nil {
		return err
	}
	var destination struct {
		PlanName  string          `json:"plan_name"`
		ActorName string          `json:"actor_name"`
		Target    json.RawMessage `json:"target"`
	}
	if objectJSON(r.text("destination_json"), &destination) != nil {
		return invalidRow()
	}
	if r["artifact_bytes"] != nil && r.integer("artifact_bytes") < 0 {
		return invalidRow()
	}
	if r["artifact_name"] != nil && (filepath.Base(r.text("artifact_name")) != r.text("artifact_name") || strings.ContainsAny(r.text("artifact_name"), "\\\x00")) {
		return invalidRow()
	}
	if r["artifact_sha256"] != nil && !validSHA256(r.text("artifact_sha256")) {
		return invalidRow()
	}
	return nil
}
func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validateCategoriesJSON(raw string) error {
	var categories []Category
	if json.Unmarshal([]byte(raw), &categories) != nil {
		return invalidRow()
	}
	_, err := ValidateCategories(categories)
	return err
}

// ParseSchedule accepts five-field crontab syntax, interpreting weekday 7 as
// Sunday without losing the wildcard marker used by the DOM/DOW OR rule.
func ParseSchedule(raw string) (cron.Schedule, error) {
	fields := strings.Fields(raw)
	if len(fields) != 5 || strings.ContainsAny(raw, "?@=") {
		return nil, invalidRow()
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	weekday, wildcard, err := normalizeWeekday(fields[4])
	if err != nil {
		return nil, err
	}
	fields[4] = weekday
	schedule, err := parser.Parse(strings.Join(fields, " "))
	if err != nil {
		return nil, invalidRow()
	}
	if wildcard {
		// Derive the parser's wildcard marker instead of depending on its private bit.
		all, _ := parser.Parse("0 0 * * *")
		explicit, _ := parser.Parse("0 0 * * 0-6")
		schedule.(*cron.SpecSchedule).Dow |= all.(*cron.SpecSchedule).Dow ^ explicit.(*cron.SpecSchedule).Dow
	}
	return schedule, nil
}

func normalizeWeekday(raw string) (string, bool, error) {
	names := map[string]int{"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}
	number := func(value string) (int, error) {
		if n, ok := names[strings.ToUpper(value)]; ok {
			return n, nil
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 7 {
			return 0, invalidRow()
		}
		return n, nil
	}
	var selected [7]bool
	wildcard := false
	for _, part := range strings.Split(raw, ",") {
		base, stride, hasStride := strings.Cut(part, "/")
		step := 1
		if hasStride {
			var err error
			step, err = strconv.Atoi(stride)
			if err != nil || step < 1 || strings.Contains(stride, "/") {
				return "", false, invalidRow()
			}
		}
		lo, hi := 0, 7
		if base == "*" {
			wildcard = true
		} else {
			first, last, isRange := strings.Cut(base, "-")
			var err error
			lo, err = number(first)
			if err != nil {
				return "", false, err
			}
			hi = lo
			if isRange {
				hi, err = number(last)
				if err != nil || hi < lo {
					return "", false, invalidRow()
				}
			} else if hasStride {
				hi = 7
			}
		}
		// At most eight iterations, even if the supplied stride is very large.
		for n := lo; n <= hi; {
			selected[n%7] = true
			if step > hi-n {
				break
			}
			n += step
		}
	}
	var values []string
	for n, yes := range selected {
		if yes {
			values = append(values, strconv.Itoa(n))
		}
	}
	if len(values) == 0 {
		return "", false, invalidRow()
	}
	return strings.Join(values, ","), wildcard, nil
}

// PlanDefinition contains only portable configuration. Filesystem ownership and
// writability checks belong to saving/enabling a plan, never archive validation.
type PlanDefinition struct {
	Name        string
	Enabled     bool
	Kind        string
	Categories  []Category
	Cron        string
	Destination json.RawMessage
	RetainCount *int
}
type ObjectDestination struct {
	Provider             string `json:"provider"`
	Endpoint             string `json:"endpoint"`
	Region               string `json:"region"`
	Bucket               string `json:"bucket"`
	Prefix               string `json:"prefix"`
	PathStyle            bool   `json:"path_style"`
	AllowPrivateNetworks bool   `json:"allow_private_networks"`
}
type ObjectCredentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
}

func (c ObjectCredentials) Validate() error {
	if strings.TrimSpace(c.AccessKeyID) == "" || strings.TrimSpace(c.SecretAccessKey) == "" || strings.ContainsAny(c.AccessKeyID+c.SecretAccessKey+c.SessionToken, "\r\n\x00") {
		return invalidRow()
	}
	return nil
}
func decodeStrictObject(raw []byte, dst any) error {
	if !utf8.Valid(raw) || !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return invalidRow()
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return invalidRow()
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return invalidRow()
	}
	return nil
}

func ValidatePlan(p PlanDefinition) error {
	if !validText(p.Name, 100) {
		return invalidRow()
	}
	if _, err := ValidateCategories(p.Categories); err != nil {
		return err
	}
	if _, err := ParseSchedule(p.Cron); err != nil {
		return err
	}
	switch p.Kind {
	case "local":
		var d struct {
			Path string `json:"path"`
		}
		if decodeStrictObject(p.Destination, &d) != nil || !filepath.IsAbs(d.Path) || strings.ContainsRune(d.Path, 0) || p.RetainCount == nil || *p.RetainCount < 1 || *p.RetainCount > 1000 {
			return invalidRow()
		}
	case "object":
		if p.RetainCount != nil {
			return invalidRow()
		}
		var d ObjectDestination
		if decodeStrictObject(p.Destination, &d) != nil {
			return invalidRow()
		}
		return d.Validate()
	default:
		return invalidRow()
	}
	return nil
}
func (d ObjectDestination) Validate() error {
	if !oneOf(d.Provider, "cos", "oss", "r2", "s3", "s3_compatible") || strings.TrimSpace(d.Region) == "" || strings.TrimSpace(d.Bucket) == "" || strings.ContainsAny(d.Bucket, "/\\\r\n\x00") {
		return invalidRow()
	}
	u, err := url.Parse(d.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(d.Endpoint, "\r\n\x00") {
		return invalidRow()
	}
	if strings.HasSuffix(u.Host, ":") {
		return invalidRow()
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return invalidRow()
		}
	}
	if d.Provider == "r2" && (d.Region != "auto" || !d.PathStyle) {
		return invalidRow()
	}
	if oneOf(d.Provider, "cos", "oss", "s3") && d.PathStyle {
		return invalidRow()
	}
	if d.Prefix != "" {
		for _, part := range strings.Split(d.Prefix, "/") {
			if part == "" || part == "." || part == ".." {
				return invalidRow()
			}
		}
	}
	if strings.ContainsFunc(d.Prefix, func(r rune) bool { return r == '\\' || unicode.IsControl(r) }) {
		return invalidRow()
	}
	return nil
}
func validatePlanRow(r payloadRow, box *secretbox.Box) error {
	if r.integer("enabled") < 0 || r.integer("enabled") > 1 {
		return invalidRow()
	}
	var categories []Category
	if json.Unmarshal([]byte(r.text("categories_json")), &categories) != nil {
		return invalidRow()
	}
	var retain *int
	if r["retain_count"] != nil {
		n := int(r.integer("retain_count"))
		retain = &n
	}
	p := PlanDefinition{Name: r.text("name"), Enabled: r.integer("enabled") == 1, Kind: r.text("kind"), Categories: categories, Cron: r.text("cron"), Destination: json.RawMessage(r.text("destination_json")), RetainCount: retain}
	if err := ValidatePlan(p); err != nil {
		return err
	}
	if p.Kind == "local" {
		if r["credentials_ciphertext"] != nil {
			return invalidRow()
		}
		return nil
	}
	plain, err := box.Open("plan:"+r.text("id"), r.blob("credentials_ciphertext"))
	if err != nil {
		return err
	}
	defer clear(plain)
	var credentials ObjectCredentials
	if decodeStrictObject(plain, &credentials) != nil {
		return invalidRow()
	}
	defer func() { credentials = ObjectCredentials{} }()
	return credentials.Validate()
}
