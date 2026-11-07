package nodes

import (
	"context"
	"orbitbeacon/internal/server/storage"
)

// MetricsMetadata 仅包含节点指标的资产标签，不读取或解码 notes。
type MetricsMetadata struct {
	ID, Name, Region, Provider, Group string
}

// MetricsMetadataRecords 用一条只读查询批量读取节点及其分组名称。
func (s *Service) MetricsMetadataRecords(ctx context.Context) ([]MetricsMetadata, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	rows, err := lease.QueryContext(ctx, `SELECT n.id,n.name,n.region,n.provider,COALESCE(g.name,'')
		FROM nodes n LEFT JOIN groups g ON g.id=n.group_id ORDER BY n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]MetricsMetadata, 0)
	for rows.Next() {
		var record MetricsMetadata
		if err := rows.Scan(&record.ID, &record.Name, &record.Region, &record.Provider, &record.Group); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
