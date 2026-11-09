package storage

import (
	"context"
	"errors"
)

// 故障注入前用真实 gate、事务日志和 ACK 固定全量前缀，不改角色或资格字段。
func ReplicaTestQuiesceAtAcknowledgedPrefix(ctx context.Context, dbs *Databases, fault func()) error {
	if dbs.replication == nil {
		return ErrUnavailable
	}
	var locked []*Database
	defer func() {
		for i := len(locked) - 1; i >= 0; i-- {
			<-locked[i].writeGate
		}
	}()
	for _, domain := range []Domain{MainDomain, HistoryDomain} {
		db := domainFrom(dbs, domain)
		select {
		case db.writeGate <- struct{}{}:
			locked = append(locked, db)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := dbs.StopReplication(); err != nil {
		return err
	}
	for _, domain := range []Domain{MainDomain, HistoryDomain} {
		d := dbs.replication.domains[domain]
		m := d.member()
		if !m.Ready || m.Rebuild != nil {
			return ErrUnavailable
		}
		if err := d.source.resolvePending(ctx); err != nil {
			return err
		}
		if err := d.catchUp(ctx); err != nil {
			return err
		}
		source, err := position(ctx, d.source.current.reader)
		if err != nil {
			return err
		}
		target, err := position(ctx, d.target.current.reader)
		if err != nil {
			return err
		}
		if source != target {
			return errors.New("mysql_fixture_did_not_ack_complete_prefix")
		}
	}
	fault()
	return nil
}
