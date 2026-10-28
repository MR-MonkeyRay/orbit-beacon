package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestBackupAndRestoreUseConsistentSQLiteSnapshot(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(dataDir, "orbitbeacon.db")
	db, err := storage.Open(context.Background(), livePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settings(key,value) VALUES ('drill','before')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	if err := os.Chmod(backupDir, 0700); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(backupDir, "orbitbeacon.db.backup")
	if err := backupDatabase(dataDir, backupPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions=%o, want 0600", info.Mode().Perm())
	}
	db, err = storage.Open(context.Background(), livePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE settings SET value='mutated' WHERE key='drill'`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(dataDir, backupPath); err != nil {
		t.Fatal(err)
	}
	db, err = storage.Open(context.Background(), livePath)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key='drill'`).Scan(&value); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if value != "before" {
		_ = db.Close()
		t.Fatalf("restored value=%q, want before", value)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	previous, err := filepath.Glob(filepath.Join(dataDir, ".orbitbeacon.db.before-restore-*"))
	if err != nil || len(previous) != 1 {
		t.Fatalf("pre-restore database was not retained: matches=%v err=%v", previous, err)
	}
	configPath := filepath.Join(t.TempDir(), "server.json")
	configData, err := json.Marshal(map[string]any{"data_dir": dataDir, "public_url": "https://backup.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatal(err)
	}
	cliBackup := filepath.Join(backupDir, "cli.backup")
	if err := run([]string{"backup", "--config", configPath, "--output", cliBackup}); err != nil {
		t.Fatalf("backup CLI wiring: %v", err)
	}
	if err := run([]string{"restore", "--data-dir", dataDir, "--input", cliBackup}); err != nil {
		t.Fatalf("restore CLI wiring: %v", err)
	}
}

func TestConcurrentBackupsDoNotDeleteWinner(t *testing.T) {
	dataDirs := []string{t.TempDir(), t.TempDir()}
	for _, dataDir := range dataDirs {
		if err := os.Chmod(dataDir, 0700); err != nil {
			t.Fatal(err)
		}
		db, err := storage.Open(context.Background(), filepath.Join(dataDir, "orbitbeacon.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	outputDir := t.TempDir()
	if err := os.Chmod(outputDir, 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(outputDir, "same.backup")
	start := make(chan struct{})
	errs := make(chan error, len(dataDirs))
	var group sync.WaitGroup
	for _, dataDir := range dataDirs {
		group.Add(1)
		go func(dataDir string) {
			defer group.Done()
			<-start
			errs <- backupDatabase(dataDir, output)
		}(dataDir)
	}
	close(start)
	group.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent backups succeeded %d times, want exactly once", successes)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("winning backup was removed: %v", err)
	}
}

func TestRestoreFailureCleansStagingArtifacts(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	inputDir := t.TempDir()
	if err := os.Chmod(inputDir, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(inputDir, "invalid.backup")
	if err := os.WriteFile(input, []byte("not a sqlite database"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(dataDir, input); err == nil {
		t.Fatal("restore accepted an invalid database")
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, ".orbitbeacon-restore-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("restore staging artifacts remain: %v", matches)
	}
}

func TestRestoreRejectsEmptyOrNonSQLiteInput(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "non-sqlite", data: []byte("not a sqlite database")},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataDir := t.TempDir()
			if err := os.Chmod(dataDir, 0700); err != nil {
				t.Fatal(err)
			}
			inputDir := t.TempDir()
			if err := os.Chmod(inputDir, 0700); err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(inputDir, "invalid.backup")
			if err := os.WriteFile(input, test.data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := restoreDatabase(dataDir, input); err == nil {
				t.Fatal("restore accepted invalid input")
			}
			if _, err := os.Lstat(filepath.Join(dataDir, "orbitbeacon.db")); !os.IsNotExist(err) {
				t.Fatalf("invalid input installed a live database: %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(dataDir, ".orbitbeacon-restore-*"))
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 0 {
				t.Fatalf("restore staging artifacts remain: %v", matches)
			}
		})
	}
}

func TestBackupRejectsMissingSourceDatabase(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	if err := os.Chmod(outputDir, 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(outputDir, "missing.backup")
	if err := backupDatabase(dataDir, output); err == nil {
		t.Fatal("backup accepted a missing source database")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("missing-source backup created output: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "server.lock")); !os.IsNotExist(err) {
		t.Fatalf("missing-source backup mutated data directory: %v", err)
	}
}

func TestRestoreReplacesCorruptLiveDatabase(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dataDir, "orbitbeacon.db")
	db, err := storage.Open(context.Background(), live)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settings(key,value) VALUES ('restore', 'from-backup')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	if err := os.Chmod(backupDir, 0700); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(backupDir, "restore.backup")
	if err := backupDatabase(dataDir, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("corrupt live database"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(dataDir, backup); err != nil {
		t.Fatal(err)
	}
	previous, err := filepath.Glob(filepath.Join(dataDir, ".orbitbeacon.db.before-restore-*"))
	if err != nil || len(previous) != 1 {
		t.Fatalf("corrupt live snapshot was not retained: matches=%v err=%v", previous, err)
	}
	db, err = storage.Open(context.Background(), live)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key='restore'`).Scan(&value); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if value != "from-backup" {
		t.Fatalf("restored value=%q, want from-backup", value)
	}
}

func TestRestoreRefusesCorruptLiveWithSidecar(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dataDir, "orbitbeacon.db")
	db, err := storage.Open(context.Background(), live)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	if err := os.Chmod(backupDir, 0700); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(backupDir, "restore.backup")
	if err := backupDatabase(dataDir, backup); err != nil {
		t.Fatal(err)
	}
	const corrupt = "corrupt live database"
	if err := os.WriteFile(live, []byte(corrupt), 0600); err != nil {
		t.Fatal(err)
	}
	journal := live + "-journal"
	if err := os.WriteFile(journal, []byte("unsafe journal"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(dataDir, backup); err == nil {
		t.Fatal("restore accepted a corrupt live database with a sidecar")
	}
	current, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != corrupt {
		t.Fatalf("failed restore modified live database: %q", current)
	}
	if _, err := os.Lstat(journal); err != nil {
		t.Fatalf("failed restore removed live sidecar: %v", err)
	}
	previous, err := filepath.Glob(filepath.Join(dataDir, ".orbitbeacon.db.before-restore-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(previous) != 0 {
		t.Fatalf("failed restore retained a partial snapshot: %v", previous)
	}
}

func TestRestoreRejectsSchemaLedgerWithoutTables(t *testing.T) {
	inputDir := t.TempDir()
	if err := os.Chmod(inputDir, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(inputDir, "invalid-schema.db")
	db, err := storage.Open(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, table := range requiredOrbitBeaconTables[:len(requiredOrbitBeaconTables)-1] {
		if _, err := db.Exec(`DROP TABLE ` + table); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(dataDir, input); err == nil {
		t.Fatal("restore accepted a schema ledger without application tables")
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "orbitbeacon.db")); !os.IsNotExist(err) {
		t.Fatalf("invalid schema installed a live database: %v", err)
	}
}

func TestRestoreRejectsBackupMissingOneMigratedTable(t *testing.T) {
	for _, table := range []string{"alert_events", "notification_channels", "notification_rate_limits", "notification_deliveries", "api_tokens"} {
		t.Run(table, func(t *testing.T) {
			inputDir, dataDir := t.TempDir(), t.TempDir()
			for _, dir := range []string{inputDir, dataDir} {
				if err := os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			input := filepath.Join(inputDir, "partial.db")
			db, err := storage.Open(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			// The ledger still claims every migration, so restore would skip
			// recreating the table and fail later at runtime.
			_, err = db.Exec(`PRAGMA foreign_keys=OFF; DROP TABLE ` + table)
			if closeErr := db.Close(); err != nil || closeErr != nil {
				t.Fatal(err, closeErr)
			}
			if err := restoreDatabase(dataDir, input); err == nil {
				t.Fatalf("restore accepted a backup without %s", table)
			}
		})
	}
}

func TestRestoreRejectsBackupMissingOrAlteredMigratedIndex(t *testing.T) {
	for name, statement := range map[string]string{
		"alert_events_active":               "DROP INDEX alert_events_active",
		"alert_events_active_predicate":     "DROP INDEX alert_events_active; CREATE UNIQUE INDEX alert_events_active ON alert_events(rule_id,node_id) WHERE state='resolved'",
		"alert_events_active_literal_case":  "DROP INDEX alert_events_active; CREATE UNIQUE INDEX alert_events_active ON alert_events(rule_id,node_id) WHERE state='FIRING'",
		"alert_events_history":              "DROP INDEX alert_events_history",
		"alert_events_state_history":        "DROP INDEX alert_events_state_history",
		"alert_events_retention":            "DROP INDEX alert_events_retention",
		"notification_deliveries_due":       "DROP INDEX notification_deliveries_due",
		"notification_deliveries_channel":   "DROP INDEX notification_deliveries_channel",
		"notification_deliveries_retention": "DROP INDEX notification_deliveries_retention",
		"notification_deliveries_history":   "DROP INDEX notification_deliveries_history",
		"api_tokens_creator":                "DROP INDEX api_tokens_creator",
		"api_tokens_revoked":                "DROP INDEX api_tokens_revoked",
		"api_tokens_expired":                "DROP INDEX api_tokens_expired",
	} {
		t.Run(name, func(t *testing.T) {
			inputDir, dataDir := t.TempDir(), t.TempDir()
			for _, dir := range []string{inputDir, dataDir} {
				if err := os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			input := filepath.Join(inputDir, "altered.db")
			db, err := storage.Open(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			// Recorded migrations skip recreation, so a missing unique index
			// would let duplicate firing events through.
			_, err = db.Exec(statement)
			if closeErr := db.Close(); err != nil || closeErr != nil {
				t.Fatal(err, closeErr)
			}
			if err := restoreDatabase(dataDir, input); err == nil {
				t.Fatalf("restore accepted a backup after %s", statement)
			}
		})
	}
}

func TestBackupSchemaContractIncludesProbeAndAlertRuleSchemas(t *testing.T) {
	for table, columns := range map[string][]string{
		"probe_latest":        {"node_id", "target", "ok", "latency_ms", "error", "status_code", "checked_at"},
		"probe_hourly":        {"node_id", "target", "hour", "checks", "failures", "latency_sum_ms"},
		"alert_rules":         {"id", "name", "enabled", "scope_kind", "scope_id", "scope_group_id", "scope_node_id", "metric", "comparator", "threshold", "for_seconds", "severity", "created_at", "updated_at"},
		"alert_rule_channels": {"rule_id", "channel_id"},
	} {
		if !containsString(requiredOrbitBeaconTables[:], table) {
			t.Errorf("required table %q is missing", table)
		}
		for _, column := range columns {
			if !containsString(requiredOrbitBeaconColumns[table], column) {
				t.Errorf("required column %q.%s is missing", table, column)
			}
		}
	}
	wantIndexes := map[string]struct {
		table   string
		columns []string
	}{
		"probe_hourly_retention":      {table: "probe_hourly", columns: []string{"hour"}},
		"alert_rules_group":           {table: "alert_rules", columns: []string{"scope_group_id"}},
		"alert_rules_node":            {table: "alert_rules", columns: []string{"scope_node_id"}},
		"alert_rule_channels_channel": {table: "alert_rule_channels", columns: []string{"channel_id"}},
	}
	for _, contract := range requiredOrbitBeaconIndexes {
		want, ok := wantIndexes[contract.name]
		if ok && contract.table == want.table && equalStrings(contract.columns, want.columns) {
			delete(wantIndexes, contract.name)
		}
	}
	for name := range wantIndexes {
		t.Errorf("%s backup schema contract is missing", name)
	}

}

func TestBackupSchemaValidationIncludesGeneratedAlertScopeColumns(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := validateDatabase(context.Background(), db); err != nil {
		t.Fatalf("migration schema rejected by backup validation: %v", err)
	}
	columns, err := tableColumns(context.Background(), db, "alert_rules")
	if err != nil {
		t.Fatal(err)
	}
	for _, generated := range []string{"scope_group_id", "scope_node_id"} {
		if !containsString(columns, generated) {
			t.Errorf("tableColumns omitted generated column alert_rules.%s", generated)
		}
	}
}

func TestRestoreRejectsFutureSchemaVersion(t *testing.T) {
	inputDir := t.TempDir()
	if err := os.Chmod(inputDir, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(inputDir, "future-schema.db")
	db, err := storage.Open(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, 'future')`, storage.CurrentSchemaVersion+1); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(dataDir, input); err == nil {
		t.Fatal("restore accepted a future schema version")
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "orbitbeacon.db")); !os.IsNotExist(err) {
		t.Fatalf("future schema installed a live database: %v", err)
	}
}

func TestRestoreRejectsMalformedIdentityIndex(t *testing.T) {
	tests := []struct {
		name  string
		index string
	}{
		{name: "wrong table with same columns", index: "CREATE UNIQUE INDEX nodes_installation_identity ON enrollment_claims(installation_id) WHERE installation_id IS NOT NULL"},
		{name: "wrong partial predicate", index: "CREATE UNIQUE INDEX nodes_installation_identity ON nodes(installation_id) WHERE 0"},
		{name: "not unique or partial", index: "CREATE INDEX nodes_installation_identity ON nodes(installation_id)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputDir := t.TempDir()
			if err := os.Chmod(inputDir, 0700); err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(inputDir, "invalid-index.db")
			db, err := storage.Open(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DROP INDEX nodes_installation_identity`); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if _, err := db.Exec(test.index); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			dataDir := t.TempDir()
			if err := os.Chmod(dataDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := restoreDatabase(dataDir, input); err == nil {
				t.Fatal("restore accepted a malformed identity index")
			}
			if _, err := os.Lstat(filepath.Join(dataDir, "orbitbeacon.db")); !os.IsNotExist(err) {
				t.Fatalf("malformed index installed a live database: %v", err)
			}
		})
	}
}

func TestNormalizeSQLKeepsLiteralsVerbatim(t *testing.T) {
	if normalizeSQL("CREATE  INDEX x ON t ( a )\n WHERE s = 'A  b';") != normalizeSQL("create index x on t(a) where s = 'A  b'") {
		t.Fatal("case or spacing outside literals changed the comparison")
	}
	for _, pair := range [][2]string{{"WHERE s='A'", "WHERE s='a'"}, {"WHERE s='a b'", "WHERE s='a  b'"}} {
		if normalizeSQL(pair[0]) == normalizeSQL(pair[1]) {
			t.Fatalf("%q compared equal to %q", pair[0], pair[1])
		}
	}
}
