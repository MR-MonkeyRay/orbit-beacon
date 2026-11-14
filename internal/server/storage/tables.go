package storage

// 固定表清单同时定义镜像传输的逻辑类型；生成列由目标引擎计算。
type ValueType uint8

const (
	Text ValueType = iota
	Integer
	Float
	Blob
)

type ColumnSpec struct {
	Name      string
	Type      ValueType
	Generated bool
}
type ForeignKeySpec struct{ Column, Table, ReferencedColumn, OnDelete string }
type TableSpec struct {
	ID             uint16
	Name           string
	Columns        []ColumnSpec
	PK             []string
	FK             []ForeignKeySpec
	MySQLGenerated []string
}

var mainTables = []TableSpec{
	{ID: 1, Name: "users", Columns: []ColumnSpec{{"id", Text, false}, {"username", Text, false}, {"password_hash", Text, false}, {"must_change_password", Integer, false}, {"totp_ciphertext", Blob, false}, {"totp_pending_ciphertext", Blob, false}, {"totp_pending_at", Text, false}, {"totp_last_step", Integer, false}, {"created_at", Text, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{}},
	{ID: 2, Name: "recovery_codes", Columns: []ColumnSpec{{"user_id", Text, false}, {"code_hash", Blob, false}}, PK: []string{"user_id", "code_hash"}, FK: []ForeignKeySpec{{"user_id", "users", "id", "CASCADE"}}},
	{ID: 3, Name: "sessions", Columns: []ColumnSpec{{"token_hash", Blob, false}, {"user_id", Text, false}, {"expires_at", Integer, false}}, PK: []string{"token_hash"}, FK: []ForeignKeySpec{{"user_id", "users", "id", "CASCADE"}}},
	{ID: 4, Name: "api_tokens", Columns: []ColumnSpec{{"id", Text, false}, {"name", Text, false}, {"token_hash", Blob, false}, {"created_by", Text, false}, {"created_at", Text, false}, {"expires_at", Text, false}, {"last_used_at", Text, false}, {"rate_limit_per_minute", Integer, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{{"created_by", "users", "id", "CASCADE"}}},
	{ID: 5, Name: "groups", Columns: []ColumnSpec{{"id", Text, false}, {"name", Text, false}, {"display_order", Integer, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{}},
	{ID: 6, Name: "nodes", Columns: []ColumnSpec{{"id", Text, false}, {"name", Text, false}, {"status", Text, false}, {"group_id", Text, false}, {"display_order", Integer, false}, {"price_amount", Text, false}, {"price_currency", Text, false}, {"billing_cycle", Text, false}, {"auto_renew", Integer, false}, {"bandwidth", Text, false}, {"ipv4", Integer, false}, {"ipv6", Integer, false}, {"network_route", Text, false}, {"plan_extra", Text, false}, {"remote_ip", Text, false}, {"geo_country", Text, false}, {"geo_location", Text, false}, {"geo_checked_at", Text, false}, {"region", Text, false}, {"provider", Text, false}, {"notes", Text, false}, {"expires_on", Text, false}, {"token_hash", Blob, false}, {"installation_id", Text, false}, {"first_enrolled_at", Text, false}, {"last_seen", Text, false}, {"credential_epoch", Integer, false}, {"system", Text, false}, {"public_settings", Text, false}, {"billing_day", Integer, false}, {"quota_bytes", Text, false}, {"billing_mode", Text, false}, {"created_at", Text, false}, {"run_generation", Text, false}, {"report_session_id", Text, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{{"group_id", "groups", "id", "SET NULL"}}},
	{ID: 7, Name: "agent_commands", Columns: []ColumnSpec{{"id", Text, false}, {"node_id", Text, false}, {"credential_epoch", Integer, false}, {"kind", Text, false}, {"update_info", Text, false}, {"state", Text, false}, {"result_code", Text, false}, {"requested_by", Text, false}, {"created_at", Text, false}, {"expires_at", Text, false}, {"delivered_at", Text, false}, {"delivered_generation", Text, false}, {"finished_at", Text, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{{"node_id", "nodes", "id", "CASCADE"}}, MySQLGenerated: []string{"active_node"}},
	{ID: 8, Name: "node_tags", Columns: []ColumnSpec{{"node_id", Text, false}, {"tag", Text, false}}, PK: []string{"node_id", "tag"}, FK: []ForeignKeySpec{{"node_id", "nodes", "id", "CASCADE"}}},
	{ID: 9, Name: "enrollment_tokens", Columns: []ColumnSpec{{"id", Text, false}, {"token_hash", Blob, false}, {"node_id", Text, false}, {"group_id", Text, false}, {"expires_at", Text, false}, {"max_uses", Integer, false}, {"uses", Integer, false}, {"revoked_at", Text, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{{"node_id", "nodes", "id", "CASCADE"}, {"group_id", "groups", "id", "SET NULL"}}},
	{ID: 10, Name: "enrollment_claims", Columns: []ColumnSpec{{"enrollment_id", Text, false}, {"installation_id", Text, false}, {"node_id", Text, false}, {"token_hash", Blob, false}, {"claimed_at", Text, false}}, PK: []string{"enrollment_id", "installation_id"}, FK: []ForeignKeySpec{{"enrollment_id", "enrollment_tokens", "id", "CASCADE"}}},
	{ID: 11, Name: "settings", Columns: []ColumnSpec{{"key", Text, false}, {"value", Text, false}}, PK: []string{"key"}, FK: []ForeignKeySpec{}},
	{ID: 12, Name: "audit_events", Columns: []ColumnSpec{{"id", Text, false}, {"actor_id", Text, false}, {"action", Text, false}, {"target_id", Text, false}, {"created_at", Text, false}, {"details", Text, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{}},
	{ID: 13, Name: "alert_rules", Columns: []ColumnSpec{{"id", Text, false}, {"name", Text, false}, {"enabled", Integer, false}, {"scope_kind", Text, false}, {"scope_id", Text, false}, {"scope_group_id", Text, true}, {"scope_node_id", Text, true}, {"metric", Text, false}, {"target", Text, false}, {"comparator", Text, false}, {"threshold", Float, false}, {"for_seconds", Integer, false}, {"severity", Text, false}, {"created_at", Text, false}, {"updated_at", Text, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{{"scope_group_id", "groups", "id", "CASCADE"}, {"scope_node_id", "nodes", "id", "CASCADE"}}},
	// IDs are stable wire identifiers; slice order follows FK dependencies.
	{ID: 36, Name: "service_groups", Columns: []ColumnSpec{{"id", Text, false}, {"name", Text, false}, {"display_order", Integer, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{}},
	{ID: 14, Name: "alert_silences", Columns: []ColumnSpec{{"id", Text, false}, {"reason", Text, false}, {"scope_kind", Text, false}, {"scope_id", Text, false}, {"scope_group_id", Text, true}, {"scope_node_id", Text, true}, {"scope_service_group_id", Text, true}, {"starts_at", Text, false}, {"ends_at", Text, false}, {"created_at", Text, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{{"scope_group_id", "groups", "id", "CASCADE"}, {"scope_node_id", "nodes", "id", "CASCADE"}, {"scope_service_group_id", "service_groups", "id", "CASCADE"}}},
	{ID: 15, Name: "alert_events", Columns: []ColumnSpec{{"id", Text, false}, {"rule_id", Text, false}, {"node_id", Text, false}, {"state", Text, false}, {"value", Float, false}, {"started_at", Text, false}, {"resolved_at", Text, false}, {"last_notified_at", Text, false}, {"rule_name", Text, false}, {"node_name", Text, false}, {"rule_snapshot", Text, false}, {"silenced", Integer, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{}, MySQLGenerated: []string{"active_rule", "active_node"}},
	{ID: 16, Name: "notification_channels", Columns: []ColumnSpec{{"id", Text, false}, {"name", Text, false}, {"kind", Text, false}, {"enabled", Integer, false}, {"config_json", Text, false}, {"secret_ciphertext", Blob, false}, {"url_ciphertext", Blob, false}, {"created_at", Text, false}, {"updated_at", Text, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{}},
	{ID: 17, Name: "notification_rate_limits", Columns: []ColumnSpec{{"channel_id", Text, false}, {"window_start", Text, false}, {"count", Integer, false}}, PK: []string{"channel_id"}, FK: []ForeignKeySpec{{"channel_id", "notification_channels", "id", "CASCADE"}}},
	{ID: 18, Name: "alert_rule_channels", Columns: []ColumnSpec{{"rule_id", Text, false}, {"channel_id", Text, false}}, PK: []string{"rule_id", "channel_id"}, FK: []ForeignKeySpec{{"rule_id", "alert_rules", "id", "CASCADE"}, {"channel_id", "notification_channels", "id", "CASCADE"}}},
	{ID: 19, Name: "notification_deliveries", Columns: []ColumnSpec{{"id", Text, false}, {"channel_id", Text, false}, {"event_id", Text, false}, {"state", Text, false}, {"attempts", Integer, false}, {"next_attempt_at", Text, false}, {"last_error_code", Text, false}, {"lease_until", Text, false}, {"payload_json", Text, false}, {"coalesced_count", Integer, false}, {"created_at", Text, false}, {"updated_at", Text, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{{"channel_id", "notification_channels", "id", "CASCADE"}}},
	{ID: 20, Name: "traffic_hourly", Columns: []ColumnSpec{{"node_id", Text, false}, {"epoch", Text, false}, {"period_start", Text, false}, {"rx_bytes", Integer, false}, {"tx_bytes", Integer, false}, {"updated_at", Text, false}}, PK: []string{"node_id", "epoch", "period_start"}, FK: []ForeignKeySpec{{"node_id", "nodes", "id", "CASCADE"}}},
	{ID: 21, Name: "traffic_daily", Columns: []ColumnSpec{{"node_id", Text, false}, {"day", Text, false}, {"rx_bytes", Integer, false}, {"tx_bytes", Integer, false}}, PK: []string{"node_id", "day"}, FK: []ForeignKeySpec{{"node_id", "nodes", "id", "CASCADE"}}},
	{ID: 22, Name: "node_availability_daily", Columns: []ColumnSpec{{"node_id", Text, false}, {"day", Text, false}, {"online_seconds", Integer, false}, {"offline_seconds", Integer, false}}, PK: []string{"node_id", "day"}, FK: []ForeignKeySpec{{"node_id", "nodes", "id", "CASCADE"}}},
	{ID: 23, Name: "probe_latest", Columns: []ColumnSpec{{"node_id", Text, false}, {"target", Text, false}, {"ok", Integer, false}, {"latency_ms", Float, false}, {"error", Text, false}, {"status_code", Integer, false}, {"checked_at", Text, false}, {"tls_expires_at", Text, false}}, PK: []string{"node_id", "target"}, FK: []ForeignKeySpec{{"node_id", "nodes", "id", "CASCADE"}}},
	{ID: 24, Name: "probe_hourly", Columns: []ColumnSpec{{"node_id", Text, false}, {"target", Text, false}, {"hour", Text, false}, {"checks", Integer, false}, {"failures", Integer, false}, {"latency_sum_ms", Float, false}, {"latency_max_ms", Float, false}}, PK: []string{"node_id", "target", "hour"}, FK: []ForeignKeySpec{{"node_id", "nodes", "id", "CASCADE"}}},
	{ID: 25, Name: "backup_plans", Columns: []ColumnSpec{{"id", Text, false}, {"name", Text, false}, {"enabled", Integer, false}, {"kind", Text, false}, {"categories_json", Text, false}, {"cron", Text, false}, {"destination_json", Text, false}, {"credentials_ciphertext", Blob, false}, {"retain_count", Integer, false}, {"next_run_at", Text, false}, {"created_at", Text, false}, {"updated_at", Text, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{}},
	{ID: 26, Name: "backup_tasks", Columns: []ColumnSpec{{"id", Text, false}, {"kind", Text, false}, {"state", Text, false}, {"phase", Text, false}, {"plan_id", Text, false}, {"actor_id", Text, false}, {"categories_json", Text, false}, {"destination_json", Text, false}, {"input_credentials_ciphertext", Blob, false}, {"scheduled_at", Text, false}, {"created_at", Text, false}, {"started_at", Text, false}, {"finished_at", Text, false}, {"artifact_name", Text, false}, {"artifact_bytes", Integer, false}, {"artifact_sha256", Text, false}, {"artifact_expires_at", Text, false}, {"artifact_deleted_at", Text, false}, {"artifact_cleanup_after", Text, false}, {"result_location", Text, false}, {"password_ciphertext", Blob, false}, {"password_expires_at", Text, false}, {"error_code", Text, false}, {"warning_code", Text, false}, {"restored", Integer, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{}, MySQLGenerated: []string{"active_plan"}},
	{ID: 27, Name: "inbox_notifications", Columns: []ColumnSpec{{"sequence", Integer, false}, {"id", Text, false}, {"kind", Text, false}, {"source_id", Text, false}, {"transition", Text, false}, {"severity", Text, false}, {"created_at", Text, false}, {"details_json", Text, false}}, PK: []string{"sequence"}, FK: []ForeignKeySpec{}},
	{ID: 28, Name: "inbox_reads", Columns: []ColumnSpec{{"user_id", Text, false}, {"notification_id", Text, false}, {"read_at", Text, false}}, PK: []string{"user_id", "notification_id"}, FK: []ForeignKeySpec{{"user_id", "users", "id", "CASCADE"}, {"notification_id", "inbox_notifications", "id", "CASCADE"}}},
	{ID: 29, Name: "inbox_sequence", Columns: []ColumnSpec{{"id", Integer, false}, {"value", Integer, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{}},
	{ID: 30, Name: "node_deletions", Columns: []ColumnSpec{{"node_id", Text, false}, {"deleted_at", Text, false}}, PK: []string{"node_id"}, FK: []ForeignKeySpec{}},
	{ID: 33, Name: "services", Columns: []ColumnSpec{{"id", Text, false}, {"name", Text, false}, {"group_id", Text, false}, {"type", Text, false}, {"target", Text, false}, {"interval_seconds", Integer, false}, {"timeout_seconds", Integer, false}, {"enabled", Integer, false}, {"public", Integer, false}, {"display_order", Integer, false}, {"notify", Integer, false}, {"latency_threshold_ms", Float, false}, {"failure_threshold", Integer, false}, {"created_at", Text, false}, {"updated_at", Text, false}}, PK: []string{"id"}, FK: []ForeignKeySpec{{"group_id", "service_groups", "id", "SET NULL"}}},
	{ID: 34, Name: "service_checks_latest", Columns: []ColumnSpec{{"service_id", Text, false}, {"result_json", Text, false}}, PK: []string{"service_id"}, FK: []ForeignKeySpec{{"service_id", "services", "id", "CASCADE"}}},
}
var historyTables = []TableSpec{
	{ID: 32, Name: "buckets", Columns: []ColumnSpec{{"node_id", Text, false}, {"metric", Text, false}, {"step", Integer, false}, {"t", Integer, false}, {"total", Float, false}, {"peak", Float, false}, {"samples", Integer, false}}, PK: []string{"node_id", "metric", "step", "t"}, FK: []ForeignKeySpec{}},
	{ID: 35, Name: "service_check_buckets", Columns: []ColumnSpec{{"service_id", Text, false}, {"t", Integer, false}, {"checks", Integer, false}, {"successes", Integer, false}, {"latency_sum_ms", Float, false}, {"latency_min_ms", Float, false}, {"latency_max_ms", Float, false}}, PK: []string{"service_id", "t"}, FK: []ForeignKeySpec{}},
}

func Tables(domain Domain) []TableSpec {
	if domain == HistoryDomain {
		return historyTables
	}
	return mainTables
}
