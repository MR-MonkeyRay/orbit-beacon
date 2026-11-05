package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/storage"
)

func TestRegistryCoversStorageSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	main, err := storage.Open(ctx, filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = main.Close() })
	metrics, err := history.OpenDatabase(ctx, filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Close() })

	// node_observations 是归档投影，实际数据来自主库 nodes，并非独立存储表。
	projectedTables := map[string]map[string]string{
		"main": {"node_observations": "nodes"},
	}
	databases := map[string]*sql.DB{"main": main, "metrics": metrics}
	registered := make(map[string]map[string]bool)
	for category, specs := range categoryTables {
		for _, spec := range specs {
			if databases[spec.Source] == nil {
				t.Fatalf("注册表 %s.%s（类别 %s）来源未知；请修正 registry.go 的 Source 或补充实际数据库比对", spec.Source, spec.Name, category)
			}
			if registered[spec.Source] == nil {
				registered[spec.Source] = make(map[string]bool)
			}
			registered[spec.Source][spec.Name] = true
		}
	}
	for source, db := range databases {
		t.Run(source, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			actual := make(map[string]bool)
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(name, "sqlite_") || (name == "schema_migrations" && !registered[source][name]) {
					continue
				}
				actual[name] = true
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			covered := make(map[string]bool)
			for name := range registered[source] {
				storageName := name
				if projected := projectedTables[source][name]; projected != "" {
					storageName = projected
				}
				if !actual[storageName] {
					t.Errorf("注册表 %s.%s 对应的实际表 %s 不存在；请修正 registry.go 或存储 schema（投影表还需核对 projectedTables 映射）", source, name, storageName)
				}
				covered[storageName] = true
			}
			for name := range actual {
				if !covered[name] && intentionallyExcluded[source][name] == "" {
					t.Errorf("实际表 %s.%s 未登记备份；请加入 registry.go，或在 intentionallyExcluded 中逐项说明有意不备份的理由", source, name)
				}
			}
		})
	}
}

func TestCommandsIntentionallyExcludedFromEveryBackupCategory(t *testing.T) {
	if intentionallyExcluded["main"]["agent_commands"] == "" {
		t.Fatal("missing exclusion rationale")
	}
	for category, specs := range categoryTables {
		for _, spec := range specs {
			if spec.Name == "agent_commands" {
				t.Fatalf("commands included in %s", category)
			}
		}
	}
}
