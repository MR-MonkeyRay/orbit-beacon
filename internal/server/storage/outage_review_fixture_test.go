package storage

import (
	"context"
	"testing"
	"time"
)

// 仅测试链接可见；时钟和控制状态驱动真实 SQLite 主备与控制记录。
type ReplicaOutageFixture struct {
	Databases *Databases
	Now       func() time.Time
	Advance   func(time.Duration)
	Manual    func(bool)
	Promote   func() error
}

func ReplicaTestOutageFixture(t *testing.T) ReplicaOutageFixture {
	t.Helper()
	dbs, domain, now := recoveryFixture(t)
	return ReplicaOutageFixture{
		Databases: dbs,
		Now:       func() time.Time { return *now },
		Advance:   func(elapsed time.Duration) { *now = now.Add(elapsed) },
		Manual:    func(required bool) { setManualIncident(t, domain, required) },
		Promote: func() error {
			ctx := context.Background()
			checkpoint, err := position(ctx, domain.target.current.reader)
			if err != nil {
				return err
			}
			if err = domain.freeze(ctx, "active_unreachable"); err != nil {
				return err
			}
			return domain.switchTo(ctx, "standby", checkpoint)
		},
	}
}
