package storage

import (
	"context"
	"database/sql"
	"strings"
)

type replicaProof struct {
	Checkpoint        *replicaPosition
	Ready, Rebuilding bool
}

type replicaCheckpoint struct {
	Instance string
	Position replicaPosition
}

func (m *memberRecord) checkpointFor(instance string) *replicaPosition {
	if m.Checkpoint != nil && m.Checkpoint.Instance == instance {
		return &m.Checkpoint.Position
	}
	return nil
}

var replicaProtocolTables = []struct{ name, columns, primary string }{
	{"replica_identity", "id:i resource_uuid:t instance_uuid:t domain:t schema_version:i", "id"},
	{"replica_state", "id:i epoch_uuid:t head_seq:i head_tx_uuid:t retained_from_seq:i capture_mode:t open_tx_uuid:t next_ordinal:i owner_connection:i journal_bytes:i ready:i", "id"},
	{"replica_epochs", "epoch_uuid:t parent_epoch_uuid:t parent_seq:i parent_tx_uuid:t", "epoch_uuid"},
	{"replica_transactions", "epoch_uuid:t seq:i tx_uuid:t previous_tx_uuid:t boot_id:t barrier_ns:i change_count:i payload_bytes:i", "epoch_uuid,seq"},
	{"replica_changes", "epoch_uuid:t seq:i ordinal:i table_id:i operation:t primary_key:j after_image:j?", "epoch_uuid,seq,ordinal"},
}

func protocolSQL(s string) string {
	return strings.NewReplacer("`", "", "\"", "", "ifnotexists", "").Replace(strings.Join(strings.Fields(strings.ToLower(s)), ""))
}
func validateProtocolDefinitions(ctx context.Context, db *Database) error {
	for i, table := range replicaProtocolTables {
		if db.current.dialect == SQLite {
			var ddl string
			if err := db.current.reader.QueryRowContext(ctx, "SELECT sql FROM sqlite_schema WHERE type='table' AND name=?", table.name).Scan(&ddl); err != nil {
				return ErrIdentityMismatch
			}
			if protocolSQL(ddl) != protocolSQL(replicaDDL(SQLite)[i]) {
				return ErrSchemaIncompatible
			}
			continue
		}
		var engine string
		if err := db.current.reader.QueryRowContext(ctx, "SELECT engine FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?", table.name).Scan(&engine); err != nil {
			return safeFailure(err)
		}
		if engine != "InnoDB" {
			return ErrSchemaIncompatible
		}
		rows, err := db.current.reader.QueryContext(ctx, "SELECT column_name,column_type,is_nullable,column_default,extra,generation_expression,character_set_name,collation_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? ORDER BY ordinal_position", table.name)
		if err != nil {
			return safeFailure(err)
		}
		expected := strings.Fields(table.columns)
		n := 0
		valid := true
		for rows.Next() {
			var name, kind, nullable, extra, generated string
			var def any
			var charset, collation sql.NullString
			if err = rows.Scan(&name, &kind, &nullable, &def, &extra, &generated, &charset, &collation); err != nil {
				rows.Close()
				return safeFailure(err)
			}
			if n >= len(expected) {
				valid = false
				continue
			}
			parts := strings.Split(expected[n], ":")
			nullableExpected := strings.HasSuffix(parts[1], "?")
			typ := "bigint"
			if parts[1] == "t" {
				typ = "varbinary(64)"
			}
			if strings.HasPrefix(parts[1], "j") {
				typ = "longtext"
			}
			valid = valid && name == parts[0] && strings.ToLower(kind) == typ && (nullable == "YES") == nullableExpected && def == nil && extra == "" && generated == ""
			if typ == "longtext" {
				valid = valid && charset.String == "utf8mb4" && collation.String == "utf8mb4_0900_bin"
			} else {
				valid = valid && !charset.Valid && !collation.Valid
			}
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return safeFailure(err)
		}
		if !valid || n != len(expected) {
			return ErrSchemaIncompatible
		}
		rows, err = db.current.reader.QueryContext(ctx, "SELECT index_name,non_unique,seq_in_index,column_name,sub_part,index_type,is_visible FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? ORDER BY index_name,seq_in_index", table.name)
		if err != nil {
			return safeFailure(err)
		}
		indexes := map[string][]string{}
		valid = true
		for rows.Next() {
			var name, kind, visible string
			var unique, ordinal int
			var column sql.NullString
			var prefix sql.NullInt64
			if err = rows.Scan(&name, &unique, &ordinal, &column, &prefix, &kind, &visible); err != nil {
				rows.Close()
				return safeFailure(err)
			}
			valid = valid && unique == 0 && ordinal == len(indexes[name])+1 && column.Valid && !prefix.Valid && kind == "BTREE" && visible == "YES"
			indexes[name] = append(indexes[name], column.String)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return safeFailure(err)
		}
		want := 1
		if table.name == "replica_transactions" {
			want = 2
			valid = valid && strings.Join(indexes["tx_uuid"], ",") == "tx_uuid"
		}
		if !valid || len(indexes) != want || strings.Join(indexes["PRIMARY"], ",") != table.primary {
			return ErrSchemaIncompatible
		}
		rows, err = db.current.reader.QueryContext(ctx, "SELECT k.column_name,k.referenced_table_name,k.referenced_column_name,k.ordinal_position,k.referenced_table_schema=DATABASE(),r.update_rule,r.delete_rule FROM information_schema.key_column_usage k JOIN information_schema.referential_constraints r ON r.constraint_schema=k.constraint_schema AND r.constraint_name=k.constraint_name AND r.table_name=k.table_name WHERE k.table_schema=DATABASE() AND k.table_name=? ORDER BY k.constraint_name,k.ordinal_position", table.name)
		if err != nil {
			return safeFailure(err)
		}
		n = 0
		valid = true
		for rows.Next() {
			var column, parent, key, update, remove string
			var ordinal int
			var local bool
			if err = rows.Scan(&column, &parent, &key, &ordinal, &local, &update, &remove); err != nil {
				rows.Close()
				return safeFailure(err)
			}
			if update == "NO ACTION" {
				update = "RESTRICT"
			}
			if remove == "NO ACTION" {
				remove = "RESTRICT"
			}
			if table.name == "replica_transactions" {
				valid = valid && n == 0 && column == "epoch_uuid" && parent == "replica_epochs" && key == "epoch_uuid" && ordinal == 1 && local && update == "RESTRICT" && remove == "RESTRICT"
			} else if table.name == "replica_changes" {
				want := "epoch_uuid"
				if n == 1 {
					want = "seq"
				}
				valid = valid && n < 2 && column == want && parent == "replica_transactions" && key == want && ordinal == n+1 && local && update == "RESTRICT" && remove == "CASCADE"
			} else {
				valid = false
			}
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return safeFailure(err)
		}
		want = 0
		if table.name == "replica_transactions" {
			want = 1
		}
		if table.name == "replica_changes" {
			want = 2
		}
		if !valid || n != want {
			return ErrSchemaIncompatible
		}
		rows, err = db.current.reader.QueryContext(ctx, "SELECT c.check_clause,t.enforced FROM information_schema.table_constraints t JOIN information_schema.check_constraints c ON c.constraint_schema=t.constraint_schema AND c.constraint_name=t.constraint_name WHERE t.table_schema=DATABASE() AND t.table_name=? AND t.constraint_type='CHECK'", table.name)
		if err != nil {
			return safeFailure(err)
		}
		n = 0
		valid = true
		for rows.Next() {
			var clause, enforced string
			if err = rows.Scan(&clause, &enforced); err != nil {
				rows.Close()
				return safeFailure(err)
			}
			clause = strings.NewReplacer("(", "", ")", "").Replace(protocolSQL(clause))
			valid = valid && clause == "id=1" && enforced == "YES"
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return safeFailure(err)
		}
		want = 0
		if table.name == "replica_identity" || table.name == "replica_state" {
			want = 1
		}
		if !valid || n != want {
			return ErrSchemaIncompatible
		}
	}
	return nil
}
