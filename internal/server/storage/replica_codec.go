package storage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxTransactionBytes int64 = 64 << 20
const maxJournalBytes int64 = 256 << 20
const transactionOverhead int64 = 256
const journalRetentionHours = 24

var ErrReplicaDiverged = errors.New("storage_replica_diverged")
var ErrIdentityMismatch = errors.New("storage_identity_mismatch")
var ErrTransactionTooLarge = errors.New("storage_transaction_too_large")
var ErrRestorePending = errors.New("storage_restore_pending")

type replicaPosition struct {
	Epoch string
	Seq   int64
	ID    string
}
type replicaChange struct {
	Ordinal   int64
	Table     uint16
	Operation string
	PK, After json.RawMessage
}
type replicaTransaction struct {
	replicaPosition
	Resource, Previous, Boot string
	Barrier                  int64
	Count, Bytes             int64
	Changes                  []replicaChange
}

// 协议身份不参与逻辑快照；实例 UUID 永远只属于所在数据库。
func replicaDDL(d Dialect) []string {
	integer, text, large, suffix := "INTEGER", "TEXT", "TEXT", ""
	if d == MySQL {
		integer, text, large, suffix = "BIGINT", "VARBINARY(64)", "LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin", " ENGINE=InnoDB"
	}
	return []string{
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS replica_identity(id %s PRIMARY KEY CHECK(id=1),resource_uuid %s NOT NULL,instance_uuid %s NOT NULL,domain %s NOT NULL,schema_version %s NOT NULL)%s", integer, text, text, text, integer, suffix),
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS replica_state(id %s PRIMARY KEY CHECK(id=1),epoch_uuid %s NOT NULL,head_seq %s NOT NULL,head_tx_uuid %s NOT NULL,retained_from_seq %s NOT NULL,capture_mode %s NOT NULL,open_tx_uuid %s NOT NULL,next_ordinal %s NOT NULL,owner_connection %s NOT NULL,journal_bytes %s NOT NULL,ready %s NOT NULL)%s", integer, text, integer, text, integer, text, text, integer, integer, integer, integer, suffix),
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS replica_epochs(epoch_uuid %s PRIMARY KEY,parent_epoch_uuid %s NOT NULL,parent_seq %s NOT NULL,parent_tx_uuid %s NOT NULL)%s", text, text, integer, text, suffix),
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS replica_transactions(epoch_uuid %s NOT NULL,seq %s NOT NULL,tx_uuid %s NOT NULL UNIQUE,previous_tx_uuid %s NOT NULL,boot_id %s NOT NULL,barrier_ns %s NOT NULL,change_count %s NOT NULL,payload_bytes %s NOT NULL,PRIMARY KEY(epoch_uuid,seq),FOREIGN KEY(epoch_uuid) REFERENCES replica_epochs(epoch_uuid))%s", text, integer, text, text, text, integer, integer, integer, suffix),
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS replica_changes(epoch_uuid %s NOT NULL,seq %s NOT NULL,ordinal %s NOT NULL,table_id %s NOT NULL,operation %s NOT NULL,primary_key %s NOT NULL,after_image %s,PRIMARY KEY(epoch_uuid,seq,ordinal),FOREIGN KEY(epoch_uuid,seq) REFERENCES replica_transactions(epoch_uuid,seq) ON DELETE CASCADE)%s", text, integer, integer, integer, text, large, large, suffix),
	}
}

func hasReplica(ctx context.Context, db *Database) (bool, error) {
	var n int
	var err error
	if db.current.dialect == MySQL {
		err = db.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('replica_identity','replica_state','replica_epochs','replica_transactions','replica_changes')").Scan(&n)
	} else {
		err = db.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type IN ('table','view') AND name IN ('replica_identity','replica_state','replica_epochs','replica_transactions','replica_changes')").Scan(&n)
	}
	if err != nil {
		return false, safeFailure(err)
	}
	if n != 0 && n != 5 {
		return false, ErrIdentityMismatch
	}
	return n == 5, nil
}
func replicaIdentity(ctx context.Context, db *Database) (resource, instance string, err error) {
	var domain string
	var version int
	err = db.current.reader.QueryRowContext(ctx, "SELECT resource_uuid,instance_uuid,domain,schema_version FROM replica_identity WHERE id=1").Scan(&resource, &instance, &domain, &version)
	expected := CurrentSchemaVersion
	if db.domain == HistoryDomain {
		expected = HistorySchemaVersion
	}
	if err != nil || domain != string(db.domain) || version != expected {
		return "", "", ErrIdentityMismatch
	}
	return
}
func installReplica(ctx context.Context, db *Database, resource, instance, epoch string, proof replicaProof) error {
	registered, err := hasReplica(ctx, db)
	if err != nil {
		return err
	}
	if registered {
		r, i, err := replicaIdentity(ctx, db)
		if err != nil || r != resource || i != instance {
			return ErrIdentityMismatch
		}
	}
	if err := validateReplicaSchema(ctx, db); err != nil {
		return err
	}
	if registered {
		if err := validateProtocolDefinitions(ctx, db); err != nil {
			return err
		}
		if err := validateProtocolState(ctx, db, proof); err != nil {
			return err
		}
	} else {
		for _, q := range replicaDDL(db.current.dialect) {
			if _, err := db.current.writer.ExecContext(ctx, q); err != nil {
				return safeFailure(err)
			}
		}
		tx, err := db.current.writer.BeginTx(ctx, nil)
		if err != nil {
			return safeFailure(err)
		}
		defer tx.Rollback()
		version := CurrentSchemaVersion
		if db.domain == HistoryDomain {
			version = HistorySchemaVersion
		}
		for _, s := range []struct {
			q    string
			args []any
		}{
			{"INSERT INTO replica_identity VALUES(1,?,?,?,?)", []any{resource, instance, string(db.domain), version}},
			{"INSERT INTO replica_epochs VALUES(?,'',0,'')", []any{epoch}},
			{"INSERT INTO replica_state VALUES(1,?,0,'',1,'off','',0,0,0,0)", []any{epoch}},
		} {
			if _, err = tx.ExecContext(ctx, s.q, s.args...); err != nil {
				return safeFailure(err)
			}
		}
		if err = tx.Commit(); err != nil {
			return safeFailure(err)
		}
	}
	return installCaptureTriggers(ctx, db)
}

func validateReplicaSchema(ctx context.Context, db *Database) error {
	known := map[string]bool{"schema_migrations": true, "storage_write_guard": true, "replica_identity": true, "replica_state": true, "replica_epochs": true, "replica_transactions": true, "replica_changes": true}
	for _, t := range Tables(db.domain) {
		known[t.Name] = true
	}
	q := "SELECT name,type FROM sqlite_schema WHERE type IN ('table','trigger') AND name NOT LIKE 'sqlite_%'"
	if db.current.dialect == MySQL {
		q = "SELECT table_name,'table' FROM information_schema.tables WHERE table_schema=DATABASE() UNION ALL SELECT trigger_name,'trigger' FROM information_schema.triggers WHERE trigger_schema=DATABASE()"
	}
	rows, err := db.current.reader.QueryContext(ctx, q)
	if err != nil {
		return safeFailure(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, kind string
		if err = rows.Scan(&name, &kind); err != nil {
			return safeFailure(err)
		}
		if kind == "table" && !known[name] {
			return ErrSchemaIncompatible
		}
		if kind == "trigger" && !expectedTrigger(db.domain, name) {
			return ErrSchemaIncompatible
		}
	}
	if err = rows.Err(); err != nil {
		return safeFailure(err)
	}
	// 核对真实 PK、基础列类型与 FK，不能给未知 DDL 安装复制捕获。
	for _, table := range Tables(db.domain) {
		if err = validateReplicaTable(ctx, db, table); err != nil {
			return err
		}
	}
	return nil
}
func expectedTrigger(domain Domain, name string) bool {
	for _, t := range Tables(domain) {
		for _, op := range []string{"i", "u", "d"} {
			if name == fmt.Sprintf("ob_replica_%d_%s", t.ID, op) {
				return true
			}
		}
	}
	return false
}
func validateReplicaTable(ctx context.Context, db *Database, table TableSpec) error {
	var rows *sql.Rows
	var err error
	if db.current.dialect == MySQL {
		rows, err = db.current.reader.QueryContext(ctx, "SELECT column_name,data_type,is_nullable,extra FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? ORDER BY ordinal_position", table.Name)
	} else {
		rows, err = db.current.reader.QueryContext(ctx, "PRAGMA table_xinfo(`"+table.Name+"`)")
	}
	if err != nil {
		return safeFailure(err)
	}
	seen := map[string]bool{}
	pk := map[int]string{}
	for rows.Next() {
		var name, kind, nullable, extra string
		var primary int
		var generated bool
		if db.current.dialect == MySQL {
			err = rows.Scan(&name, &kind, &nullable, &extra)
			generated = strings.Contains(extra, "STORED GENERATED") || strings.Contains(extra, "VIRTUAL GENERATED")
		} else {
			var cid, notnull, hidden int
			var def any
			err = rows.Scan(&cid, &name, &kind, &notnull, &def, &primary, &hidden)
			generated = hidden != 0
			if primary > 0 {
				pk[primary] = name
			}
		}
		if err != nil {
			rows.Close()
			return safeFailure(err)
		}
		if db.current.dialect == MySQL && generated {
			extra := false
			for _, nameExpected := range table.MySQLGenerated {
				if name == nameExpected {
					extra = true
					seen[name] = true
				}
			}
			if extra {
				continue
			}
		}
		found := false
		for _, c := range table.Columns {
			if c.Name == name {
				found = true
				seen[name] = true
				if c.Generated != generated {
					rows.Close()
					return ErrSchemaIncompatible
				}
				k := strings.ToLower(kind)
				valid := false
				switch c.Type {
				case Integer:
					valid = strings.Contains(k, "int")
				case Float:
					valid = k == "real" || k == "double"
				case Blob:
					valid = strings.Contains(k, "blob") || k == "varbinary" || k == "binary"
				case Text:
					valid = strings.Contains(k, "text") || strings.Contains(k, "char") || k == "varbinary"
				}
				if !valid {
					rows.Close()
					return ErrSchemaIncompatible
				}
			}
		}
		if !found {
			rows.Close()
			return ErrSchemaIncompatible
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return safeFailure(err)
	}
	expected := len(table.Columns)
	if db.current.dialect == MySQL {
		expected += len(table.MySQLGenerated)
	}
	if len(seen) != expected {
		return ErrSchemaIncompatible
	}
	if db.current.dialect == MySQL {
		rows, err = db.current.reader.QueryContext(ctx, "SELECT column_name,seq_in_index FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name='PRIMARY' ORDER BY seq_in_index", table.Name)
		if err != nil {
			return safeFailure(err)
		}
		for rows.Next() {
			var name string
			var n int
			if err = rows.Scan(&name, &n); err != nil {
				rows.Close()
				return safeFailure(err)
			}
			pk[n] = name
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return safeFailure(err)
		}
	}
	if len(pk) != len(table.PK) {
		return ErrSchemaIncompatible
	}
	for i, name := range table.PK {
		if pk[i+1] != name {
			return ErrSchemaIncompatible
		}
	}
	var actual []ForeignKeySpec
	if db.current.dialect == MySQL {
		rows, err = db.current.reader.QueryContext(ctx, "SELECT k.column_name,k.referenced_table_name,k.referenced_column_name,r.delete_rule FROM information_schema.key_column_usage k JOIN information_schema.referential_constraints r ON r.constraint_schema=k.constraint_schema AND r.constraint_name=k.constraint_name AND r.table_name=k.table_name WHERE k.table_schema=DATABASE() AND k.table_name=? AND k.referenced_table_name IS NOT NULL", table.Name)
	} else {
		rows, err = db.current.reader.QueryContext(ctx, "PRAGMA foreign_key_list(`"+table.Name+"`)")
	}
	if err != nil {
		return safeFailure(err)
	}
	for rows.Next() {
		var f ForeignKeySpec
		if db.current.dialect == MySQL {
			err = rows.Scan(&f.Column, &f.Table, &f.ReferencedColumn, &f.OnDelete)
		} else {
			var id, seq int
			var onupdate, match string
			err = rows.Scan(&id, &seq, &f.Table, &f.Column, &f.ReferencedColumn, &onupdate, &f.OnDelete, &match)
		}
		if err != nil {
			rows.Close()
			return safeFailure(err)
		}
		actual = append(actual, f)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return safeFailure(err)
	}
	if len(actual) != len(table.FK) {
		return ErrSchemaIncompatible
	}
	for _, want := range table.FK {
		found := false
		for _, got := range actual {
			if got == want {
				found = true
			}
		}
		if !found {
			return ErrSchemaIncompatible
		}
	}
	return nil
}

func jsonImage(d Dialect, table TableSpec, prefix string, pk bool) string {
	var parts []string
	cols := columns(table)
	if pk {
		cols = nil
		for _, name := range table.PK {
			for _, c := range table.Columns {
				if c.Name == name {
					cols = append(cols, c)
				}
			}
		}
	}
	for _, c := range cols {
		v := prefix + ".`" + c.Name + "`"
		var tag, value string
		switch c.Type {
		case Text:
			tag = "t"
			value = "hex(" + v + ")"
		case Blob:
			tag = "b"
			value = "hex(" + v + ")"
		case Integer:
			tag = "i"
			value = "CAST(" + v + " AS TEXT)"
			if d == MySQL {
				value = "CAST(" + v + " AS CHAR CHARACTER SET utf8mb4)"
			}
		case Float:
			tag = "f"
			value = "printf('%!.26g'," + v + ")"
			if d == MySQL {
				value = "CAST(JSON_EXTRACT(JSON_ARRAY(" + v + "),'$[0]') AS CHAR CHARACTER SET utf8mb4)"
			}
		}
		expr := "CASE WHEN " + v + " IS NULL THEN NULL ELSE json_array('" + tag + "'," + value + ") END"
		parts = append(parts, expr)
	}
	return "json_array(" + strings.Join(parts, ",") + ")"
}
func installCaptureTriggers(ctx context.Context, db *Database) error {
	d := db.current.dialect
	for _, table := range Tables(db.domain) {
		for _, op := range []string{"i", "u", "d"} {
			name := fmt.Sprintf("ob_replica_%d_%s", table.ID, op)
			event := map[string]string{"i": "INSERT", "u": "UPDATE", "d": "DELETE"}[op]
			prefix := "NEW"
			if op == "d" {
				prefix = "OLD"
			}
			pk := jsonImage(d, table, prefix, true)
			after := "NULL"
			if op != "d" {
				after = jsonImage(d, table, "NEW", false)
			}
			var changed []string
			for _, col := range table.PK {
				if d == MySQL {
					changed = append(changed, "NOT (OLD.`"+col+"` <=> NEW.`"+col+"`)")
				} else {
					changed = append(changed, "OLD.`"+col+"` IS NOT NEW.`"+col+"`")
				}
			}
			var q string
			if d == SQLite {
				q = "CREATE TRIGGER IF NOT EXISTS `" + name + "` AFTER " + event + " ON `" + table.Name + "` BEGIN "
				if op == "u" {
					q += "SELECT CASE WHEN " + strings.Join(changed, " OR ") + " THEN RAISE(ABORT,'storage_primary_key_immutable') END; "
				}
				q += "SELECT CASE WHEN (SELECT capture_mode FROM replica_state WHERE id=1)<>'off' AND (SELECT open_tx_uuid='' FROM replica_state WHERE id=1) THEN RAISE(ABORT,'storage_capture_context_required') END; "
				q += "INSERT INTO replica_changes(epoch_uuid,seq,ordinal,table_id,operation,primary_key,after_image) SELECT epoch_uuid,head_seq+1,next_ordinal," + strconv.Itoa(int(table.ID)) + ",'" + op + "'," + pk + "," + after + " FROM replica_state WHERE id=1 AND capture_mode='source'; "
				q += "UPDATE replica_transactions SET change_count=change_count+1,payload_bytes=payload_bytes+length(CAST(" + pk + " AS BLOB))+COALESCE(length(CAST(" + after + " AS BLOB)),0)+64 WHERE tx_uuid=(SELECT open_tx_uuid FROM replica_state WHERE id=1) AND (SELECT capture_mode FROM replica_state WHERE id=1)='source'; "
				q += "UPDATE replica_state SET next_ordinal=next_ordinal+1 WHERE id=1 AND capture_mode='source'; SELECT CASE WHEN (SELECT COALESCE(max(payload_bytes),0) FROM replica_transactions WHERE tx_uuid=(SELECT open_tx_uuid FROM replica_state WHERE id=1))>67108864 THEN RAISE(FAIL,'storage_transaction_too_large') END; END"
			} else {
				q = "CREATE TRIGGER `" + name + "` AFTER " + event + " ON `" + table.Name + "` FOR EACH ROW BEGIN "
				if op == "u" {
					q += "IF " + strings.Join(changed, " OR ") + " THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='storage_primary_key_immutable'; END IF; "
				}
				q += "IF (SELECT capture_mode<>'off' AND (open_tx_uuid='' OR owner_connection<>CONNECTION_ID()) FROM replica_state WHERE id=1) THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='storage_capture_context_required'; END IF; "
				q += "IF (SELECT capture_mode='source' FROM replica_state WHERE id=1) THEN INSERT INTO replica_changes(epoch_uuid,seq,ordinal,table_id,operation,primary_key,after_image) SELECT epoch_uuid,head_seq+1,next_ordinal," + strconv.Itoa(int(table.ID)) + ",'" + op + "'," + pk + "," + after + " FROM replica_state WHERE id=1; "
				q += "UPDATE replica_transactions SET change_count=change_count+1,payload_bytes=payload_bytes+OCTET_LENGTH(" + pk + ")+COALESCE(OCTET_LENGTH(" + after + "),0)+64 WHERE tx_uuid=(SELECT open_tx_uuid FROM replica_state WHERE id=1); UPDATE replica_state SET next_ordinal=next_ordinal+1 WHERE id=1; IF (SELECT payload_bytes>67108864 FROM replica_transactions WHERE tx_uuid=(SELECT open_tx_uuid FROM replica_state WHERE id=1)) THEN SIGNAL SQLSTATE '45000' SET MYSQL_ERRNO=60001,MESSAGE_TEXT='storage_transaction_too_large'; END IF; END IF; END"
			}
			if err := ensureCaptureTrigger(ctx, db, name, q); err != nil {
				return mysqlStartupFailure(db.domain, "capture_trigger", int(table.ID), err)
			}
		}
	}
	return nil
}

func decodeValues(raw []byte, cols []ColumnSpec) ([]any, error) {
	var encoded []json.RawMessage
	if err := json.Unmarshal(raw, &encoded); err != nil || len(encoded) != len(cols) {
		return nil, ErrReplicaDiverged
	}
	out := make([]any, len(cols))
	for i, c := range cols {
		if string(encoded[i]) == "null" {
			continue
		}
		var pair []string
		if err := json.Unmarshal(encoded[i], &pair); err != nil || len(pair) != 2 {
			return nil, ErrReplicaDiverged
		}
		var err error
		switch c.Type {
		case Text, Blob:
			tag := "t"
			if c.Type == Blob {
				tag = "b"
			}
			if pair[0] != tag {
				return nil, ErrReplicaDiverged
			}
			var b []byte
			b, err = hex.DecodeString(pair[1])
			if c.Type == Text {
				if !utf8.Valid(b) {
					return nil, ErrReplicaDiverged
				}
				out[i] = string(b)
			} else {
				if b == nil {
					b = []byte{}
				}
				out[i] = b
			}
		case Integer:
			if pair[0] != "i" {
				return nil, ErrReplicaDiverged
			}
			out[i], err = strconv.ParseInt(pair[1], 10, 64)
		case Float:
			if pair[0] != "f" {
				return nil, ErrReplicaDiverged
			}
			var f float64
			f, err = strconv.ParseFloat(pair[1], 64)
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, ErrReplicaDiverged
			}
			out[i] = f
		default:
			return nil, ErrReplicaDiverged
		}
		if err != nil {
			return nil, ErrReplicaDiverged
		}
	}
	return out, nil
}
func primaryColumns(table TableSpec) []ColumnSpec {
	var cols []ColumnSpec
	for _, key := range table.PK {
		for _, c := range table.Columns {
			if c.Name == key {
				cols = append(cols, c)
			}
		}
	}
	return cols
}
func position(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (replicaPosition, error) {
	var p replicaPosition
	err := q.QueryRowContext(ctx, "SELECT epoch_uuid,head_seq,head_tx_uuid FROM replica_state WHERE id=1").Scan(&p.Epoch, &p.Seq, &p.ID)
	return p, safeFailure(err)
}
