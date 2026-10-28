package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"orbitbeacon/internal/server/securefs"
)

type columnSpec struct {
	Name string
	Type string
	PK   int
}
type tableSpec struct {
	Name    string
	Columns []columnSpec
	Source  string
	Where   string
}

func table(name, source, where, definition, primary string) tableSpec {
	keys := strings.Split(primary, ",")
	columns := strings.Split(definition, ",")
	spec := tableSpec{Name: name, Source: source, Where: where}
	for _, column := range columns {
		parts := strings.Fields(column)
		c := columnSpec{Name: parts[0], Type: parts[1]}
		for i, key := range keys {
			if c.Name == key {
				c.PK = i + 1
			}
		}
		spec.Columns = append(spec.Columns, c)
	}
	return spec
}

var categoryTables = map[Category][]tableSpec{
	Configuration: {
		table("users", "main", "", "id TEXT,username TEXT,password_hash TEXT,created_at TEXT", "id"),
		table("groups", "main", "", "id TEXT,name TEXT", "id"),
		table("nodes", "main", "", "id TEXT,name TEXT,status TEXT,group_id TEXT,region TEXT,provider TEXT,notes TEXT,created_at TEXT,public_settings TEXT,billing_day INTEGER,quota_bytes TEXT,token_hash BLOB,credential_epoch INTEGER,installation_id TEXT,first_enrolled_at TEXT,run_generation TEXT,report_session_id TEXT", "id"),
		table("node_tags", "main", "", "node_id TEXT,tag TEXT", "node_id,tag"),
		table("alert_rules", "main", "", "id TEXT,name TEXT,enabled INTEGER,scope_kind TEXT,scope_id TEXT,metric TEXT,comparator TEXT,threshold REAL,for_seconds INTEGER,severity TEXT,created_at TEXT,updated_at TEXT", "id"),
		table("notification_channels", "main", "", "id TEXT,name TEXT,kind TEXT,enabled INTEGER,config_json TEXT,secret_ciphertext BLOB,url_ciphertext BLOB,created_at TEXT,updated_at TEXT", "id"),
		table("alert_rule_channels", "main", "", "rule_id TEXT,channel_id TEXT", "rule_id,channel_id"),
		table("backup_plans", "main", "", "id TEXT,name TEXT,enabled INTEGER,kind TEXT,categories_json TEXT,cron TEXT,destination_json TEXT,credentials_ciphertext BLOB,retain_count INTEGER,created_at TEXT,updated_at TEXT", "id"),
	},
	Monitoring: {
		table("node_observations", "main", "", "node_id TEXT,last_seen TEXT,system TEXT,agent_runtime TEXT", "node_id"),
		table("traffic_hourly", "main", "", "node_id TEXT,epoch TEXT,period_start TEXT,rx_bytes INTEGER,tx_bytes INTEGER,updated_at TEXT", "node_id,epoch,period_start"),
		table("traffic_daily", "main", "", "node_id TEXT,day TEXT,rx_bytes INTEGER,tx_bytes INTEGER", "node_id,day"),
		table("probe_latest", "main", "", "node_id TEXT,target TEXT,ok INTEGER,latency_ms REAL,error TEXT,status_code INTEGER,checked_at TEXT", "node_id,target"),
		table("probe_hourly", "main", "", "node_id TEXT,target TEXT,hour TEXT,checks INTEGER,failures INTEGER,latency_sum_ms REAL", "node_id,target,hour"),
		table("buckets", "metrics", "", "node_id TEXT,metric TEXT,step INTEGER,t INTEGER,total REAL,peak REAL,samples INTEGER", "node_id,metric,step,t"),
	},
	Events: {
		table("audit_events", "main", "", "id TEXT,actor_id TEXT,action TEXT,target_id TEXT,created_at TEXT,details TEXT", "id"),
		table("alert_events", "main", "", "id TEXT,rule_id TEXT,node_id TEXT,state TEXT,value REAL,started_at TEXT,resolved_at TEXT,last_notified_at TEXT,rule_name TEXT,node_name TEXT,rule_snapshot TEXT", "id"),
		table("notification_deliveries", "main", "state IN ('sent','failed')", "id TEXT,channel_id TEXT,event_id TEXT,state TEXT,attempts INTEGER,next_attempt_at TEXT,last_error_code TEXT,lease_until TEXT,payload_json TEXT,coalesced_count INTEGER,created_at TEXT,updated_at TEXT", "id"),
		table("inbox_notifications", "main", "", "id TEXT,kind TEXT,source_id TEXT,transition TEXT,severity TEXT,created_at TEXT,details_json TEXT", "id"),
		table("inbox_reads", "main", "", "user_id TEXT,notification_id TEXT,read_at TEXT", "user_id,notification_id"),
		table("backup_tasks", "main", "state IN ('succeeded','failed')", "id TEXT,kind TEXT,state TEXT,phase TEXT,plan_id TEXT,actor_id TEXT,categories_json TEXT,destination_json TEXT,scheduled_at TEXT,created_at TEXT,started_at TEXT,finished_at TEXT,artifact_name TEXT,artifact_bytes INTEGER,artifact_sha256 TEXT,result_location TEXT,error_code TEXT,warning_code TEXT,restored INTEGER", "id"),
	},
}

func tablesFor(category Category) []tableSpec { return categoryTables[category] }
func columnNames(spec tableSpec) string {
	names := make([]string, len(spec.Columns))
	for i, c := range spec.Columns {
		names[i] = c.Name
	}
	return strings.Join(names, ",")
}

// The transfer format has one fixed DDL per table. Reject archive-controlled
// expressions before integrity_check can evaluate CHECK constraints.
func payloadTableDDL(spec tableSpec) string {
	columns := make([]string, 0, len(spec.Columns)+1)
	var keys []string
	for _, c := range spec.Columns {
		columns = append(columns, c.Name+" "+c.Type)
		if c.PK > 0 {
			keys = append(keys, c.Name)
		}
	}
	if len(keys) > 0 {
		columns = append(columns, "PRIMARY KEY("+strings.Join(keys, ",")+")")
	}
	return "CREATE TABLE " + spec.Name + " (" + strings.Join(columns, ",") + ")"
}

func createPayload(ctx context.Context, path string, category Category) (*sql.DB, error) {
	specs := tablesFor(category)
	if len(specs) == 0 {
		return nil, ErrInvalidCategories
	}
	file, err := securefs.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	u.RawQuery = url.Values{"_pragma": {"journal_mode(DELETE)", "trusted_schema(OFF)", "synchronous(FULL)", "cache_size(-8192)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, spec := range specs {
		if _, err = db.ExecContext(ctx, payloadTableDDL(spec)); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

func openPayload(ctx context.Context, path string) (*sql.DB, error) {
	exists, err := securefs.ValidateFile(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, os.ErrNotExist
	}
	u := url.URL{Scheme: "file", Path: path}
	u.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"query_only(ON)", "trusted_schema(OFF)", "busy_timeout(5000)", "cache_size(-8192)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ValidatePayloadSchema inspects only SQLite's metadata before any row query.
// Archive SQL never supplies identifiers or DDL for the final database.
func ValidatePayloadSchema(ctx context.Context, db *sql.DB, category Category) error {
	specs := tablesFor(category)
	if len(specs) == 0 {
		return ErrInvalidCategories
	}
	if _, err := db.ExecContext(ctx, "PRAGMA trusted_schema=OFF"); err != nil {
		return err
	}
	expected := make(map[string]tableSpec, len(specs))
	for _, s := range specs {
		expected[s.Name] = s
	}
	rows, err := db.QueryContext(ctx, `SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name`)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(specs))
	for rows.Next() {
		var kind, name, owner string
		var ddl sql.NullString
		if err = rows.Scan(&kind, &name, &owner, &ddl); err != nil {
			rows.Close()
			return err
		}
		switch kind {
		case "table":
			spec, ok := expected[name]
			if !ok || name != owner || !ddl.Valid || ddl.String != payloadTableDDL(spec) {
				rows.Close()
				return errors.New("restore_invalid_data: unexpected payload table")
			}
			seen[name] = true
		case "index":
			if _, ok := expected[owner]; !ok || ddl.Valid || !strings.HasPrefix(name, "sqlite_autoindex_"+owner+"_") {
				rows.Close()
				return errors.New("restore_invalid_data: unexpected payload index")
			}
		default:
			rows.Close()
			return errors.New("restore_invalid_data: executable payload schema")
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return errors.New("restore_invalid_data: missing payload tables")
	}
	for _, spec := range specs {
		columns, err := db.QueryContext(ctx, "PRAGMA table_xinfo("+spec.Name+")")
		if err != nil {
			return err
		}
		count := 0
		for columns.Next() {
			var cid, notnull, pk, hidden int
			var name, kind string
			var def any
			if err = columns.Scan(&cid, &name, &kind, &notnull, &def, &pk, &hidden); err != nil {
				columns.Close()
				return err
			}
			if count >= len(spec.Columns) || hidden != 0 || def != nil {
				columns.Close()
				return fmt.Errorf("restore_invalid_data: payload columns %s", spec.Name)
			}
			want := spec.Columns[count]
			if cid != count || name != want.Name || kind != want.Type || pk != want.PK {
				columns.Close()
				return fmt.Errorf("restore_invalid_data: payload columns %s", spec.Name)
			}
			count++
		}
		if err = errors.Join(columns.Err(), columns.Close()); err != nil {
			return err
		}
		if count != len(spec.Columns) {
			return fmt.Errorf("restore_invalid_data: payload columns %s", spec.Name)
		}
		indexes, err := db.QueryContext(ctx, "PRAGMA index_list("+spec.Name+")")
		if err != nil {
			return err
		}
		n := 0
		for indexes.Next() {
			var seq, unique, partial int
			var name, origin string
			if err = indexes.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
				indexes.Close()
				return err
			}
			if unique != 1 || partial != 0 || origin != "pk" {
				indexes.Close()
				return fmt.Errorf("restore_invalid_data: payload index %s", spec.Name)
			}
			n++
		}
		if err = errors.Join(indexes.Err(), indexes.Close()); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("restore_invalid_data: payload primary key %s", spec.Name)
		}
		fk, err := db.QueryContext(ctx, "PRAGMA foreign_key_list("+spec.Name+")")
		if err != nil {
			return err
		}
		hasFK := fk.Next()
		err = errors.Join(fk.Err(), fk.Close())
		if err != nil {
			return err
		}
		if hasFK {
			return errors.New("restore_invalid_data: payload foreign key")
		}
	}
	return nil
}
