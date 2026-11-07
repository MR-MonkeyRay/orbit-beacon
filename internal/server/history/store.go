package history

import (
	"context"
	"database/sql"
	"time"
)

// Bucket 保存一个序列在一分钟内的聚合样本。
type Bucket struct {
	Node, Series string
	T            int64 // 分钟起点，Unix 秒
	Total, Peak  float64
	Samples      int64
}

type Store interface {
	// Write 在同一源事务持久化三档聚合；失败时也返回计划写入的存储行数。
	// ErrCommitUnknown 后仅允许原批次重入，先解析原事务，确定回滚后才重做。
	Write(ctx context.Context, buckets []Bucket) (rows int, err error)
	// Query 返回闭区间内按时间升序排列的点。
	Query(ctx context.Context, node, series string, stepSeconds int, start, end int64) ([]Point, error)
	// Series 返回已存储的序列名称，供重启后恢复路径准入清单。
	Series(ctx context.Context, node string) ([]string, error)
	// NodeIDs 访问所有有历史记录的节点；visit 可以在行之间运行。
	NodeIDs(ctx context.Context, visit func(string)) error
	// DeleteNode 和 DeleteExpired 在批次之间调用 yield，使生产者队列持续排空。
	DeleteNode(ctx context.Context, node string, yield func()) (int64, error)
	DeleteExpired(ctx context.Context, now time.Time, yield func()) (int64, error)
	// DatabaseStats 返回连接池快照；没有连接池的存储返回 ok=false。
	DatabaseStats() (writer, reader sql.DBStats, ok bool)
	Close() error
}
