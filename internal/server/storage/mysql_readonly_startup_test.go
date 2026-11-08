package storage

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
)

func TestMySQLStartupReadOnlyIsRecoverableButImmutableDeploymentErrorsRemainManual(t *testing.T) {
	for _, test := range []struct {
		name                               string
		durable, binlog, syncBinlog, trust int64
		support, transactions              string
		manual                             bool
	}{
		{"read_only", 1, 1, 1, 1, "DEFAULT", "YES", false},
		{"read_only_without_binlog", 1, 0, 0, 0, "DEFAULT", "YES", false},
		{"read_only_and_unsafe_durability", 2, 1, 1, 1, "DEFAULT", "YES", true},
		{"read_only_and_unsafe_binlog", 1, 1, 0, 1, "DEFAULT", "YES", true},
		{"read_only_and_missing_trigger_prerequisite", 1, 1, 1, 0, "DEFAULT", "YES", true},
		{"read_only_and_no_innodb", 1, 1, 1, 1, "NO", "NO", true},
		{"read_only_and_no_transactions", 1, 1, 1, 1, "YES", "NO", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool := sql.OpenDB(&deploymentConnector{variables: []driver.Value{test.durable, test.binlog, test.syncBinlog, test.trust, int64(1)}, engine: []driver.Value{test.support, test.transactions}})
			defer pool.Close()
			failure := checkMySQLDeployment(t.Context(), pool, MainDomain)
			if failure == nil {
				t.Fatal("read-only endpoint admitted writes")
			}
			if errors.Is(failure, ErrDeployment) != test.manual || storageFailure(failure) == test.manual {
				t.Fatalf("deployment classification: manual=%v recoverable=%v wantManual=%v", errors.Is(failure, ErrDeployment), storageFailure(failure), test.manual)
			}
			d, m := failoverPair(t)
			if err := d.startupFailure(t.Context(), failure); err != nil {
				t.Fatal(err)
			}
			if m.Manual != test.manual || m.Freeze == nil || d.source.Ready() {
				t.Fatal("startup did not preserve the correct recovery policy", m.Manual, m.Freeze, d.source.Ready())
			}
			if !test.manual && !m.Ready {
				t.Fatal("temporary read-only source revoked its healthy complete standby")
			}
		})
	}
}
