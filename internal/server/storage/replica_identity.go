package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"

	"orbitbeacon/internal/config"
)

// 先检查已登记身份，再运行任何可能初始化 schema 的入口。
func inspectMember(ctx context.Context, e config.DatabaseEndpoint, domain Domain, dataDir string) (string, string, error) {
	var pool *sql.DB
	var err error
	if e.Driver == "mysql" {
		cfg, e := mysqlConfig(e, domain)
		if e != nil {
			return "", "", e
		}
		pool, err = mysqlConnectorDB(cfg)
	} else {
		path := filepath.Join(dataDir, "orbitbeacon.db")
		if domain == HistoryDomain {
			path = filepath.Join(dataDir, "metrics.db")
		}
		pool, err = OpenReader(ctx, path)
	}
	if err != nil {
		return "", "", err
	}
	defer pool.Close()
	var resource, instance, actual string
	var version int
	err = pool.QueryRowContext(ctx, "SELECT resource_uuid,instance_uuid,domain,schema_version FROM replica_identity WHERE id=1").Scan(&resource, &instance, &actual, &version)
	if err != nil {
		if IsConnectionFailure(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", "", safeFailure(err)
		}
		return "", "", ErrIdentityMismatch
	}
	expected := CurrentSchemaVersion
	if domain == HistoryDomain {
		expected = HistorySchemaVersion
	}
	if actual != string(domain) || version != expected {
		return "", "", ErrIdentityMismatch
	}
	return resource, instance, nil
}
