package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

type publicStreamStore struct {
	mu            sync.Mutex
	queries       int
	queryErr      error
	queryWait     <-chan struct{}
	onQuery       func()
	queryExitGate <-chan struct{}
	closed        chan struct{}
}

var streamQueryStores sync.Map
var streamQuerySequence atomic.Int64

func init() {
	sqlite.MustRegisterScalarFunction("server_test_stream_gate", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		value, ok := streamQueryStores.Load(args[0].(int64))
		if !ok {
			return nil, errors.New("stream fixture closed")
		}
		store := value.(*publicStreamStore)
		store.mu.Lock()
		first := args[1].(int64) == 1
		if first {
			store.queries++
		}
		err, wait, onQuery := store.queryErr, store.queryWait, store.onQuery
		exit, closed := store.queryExitGate, store.closed
		store.mu.Unlock()
		if first && onQuery != nil {
			onQuery()
		}
		if err != nil {
			return nil, err
		}
		// 阻塞函数用于排空屏障；其余等待留在递归 SQL 内，交由真实 context 中断。
		if first && exit != nil {
			select {
			case <-exit:
			case <-closed:
			}
			return int64(1), nil
		}
		if wait == nil {
			return int64(1), nil
		}
		select {
		case <-wait:
			return int64(1), nil
		case <-closed:
			return int64(1), nil
		default:
			return int64(0), nil
		}
	})
}

func installStreamQueryFixture(t *testing.T, db *sql.DB, h *handler, store *publicStreamStore) {
	t.Helper()
	key := streamQuerySequence.Add(1)
	store.closed = make(chan struct{})
	streamQueryStores.Store(key, store)
	t.Cleanup(func() {
		close(store.closed)
		h.CloseLiveStreams()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = h.WaitLiveStreams(ctx)
		streamQueryStores.Delete(key)
	})
	if _, err := db.Exec("INSERT INTO nodes(id,name,created_at) SELECT ?,?,? WHERE NOT EXISTS(SELECT 1 FROM nodes)", "00000000-0000-4000-8000-000000000001", "Private node", timekey.Format(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE nodes RENAME TO stream_fixture_nodes`); err != nil {
		t.Fatal(err)
	}
	query := fmt.Sprintf(`CREATE VIEW nodes AS SELECT source.* FROM
		(WITH RECURSIVE gate(ready) AS (
			SELECT server_test_stream_gate(%d,1)
			UNION ALL SELECT server_test_stream_gate(%d,0) FROM gate WHERE ready=0
		) SELECT ready FROM gate WHERE ready=1 LIMIT 1) AS barrier
		CROSS JOIN stream_fixture_nodes AS source`, key, key)
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func setStreamPublicSettings(t *testing.T, db *sql.DB, visible bool, name string) {
	t.Helper()
	settings, err := json.Marshal(nodes.PublicSettings{Enabled: visible, Fields: nodes.PublicFields})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE stream_fixture_nodes SET public_settings=?,name=?,region='Earth',provider='Test'", string(settings), name); err != nil {
		t.Fatal(err)
	}
}
