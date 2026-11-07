package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func mysqlHealthRequest(h *handler, path string) int {
	response := httptest.NewRecorder()
	h.health(response, httptest.NewRequest(http.MethodGet, path, nil), "")
	return response.Code
}

func TestMySQLRequestDeadlineRollsBackWithoutPoisoningReadiness(t *testing.T) {
	f := storagetest.Start(t)
	db := f.Open(t, storage.MainDomain)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lease, err := db.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(lease.Context(), "INSERT INTO settings(`key`,value) VALUES('deadline_probe','uncommitted')"); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	if err = tx.Commit(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired request commit=%v", err)
	}
	lease.Close()
	if !db.Ready() {
		t.Fatal("rolled-back request deadline poisoned database readiness")
	}
	committed, err := tx.ResolveCommit(context.Background())
	if err != nil || committed {
		t.Fatalf("deadline receipt committed=%v err=%v", committed, err)
	}
	reader, err := db.Acquire(context.Background(), storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var count int
	if err = reader.QueryRowContext(reader.Context(), "SELECT count(*) FROM settings WHERE `key`='deadline_probe'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired request persisted count=%d err=%v", count, err)
	}
}

func mysqlHealthProxyDatabase(t *testing.T, inspectCommit bool) (*storage.Database, *mysqlHealthProxy) {
	t.Helper()
	f := storagetest.Start(t)
	_ = f.Open(t, storage.MainDomain)
	endpoint := f.Endpoint("primary")
	if inspectCommit && endpoint.TLS != nil {
		t.Skip("COMMIT-response fault injection requires the plaintext MySQL profile; TLS packets cannot be inspected")
	}
	proxy := newMySQLHealthProxy(t, endpoint)
	if endpoint.TLS != nil && endpoint.TLS.ServerName == "" {
		tls := *endpoint.TLS
		tls.ServerName, _, _ = net.SplitHostPort(endpoint.Address)
		endpoint.TLS = &tls
	}
	endpoint.Network, endpoint.Address = "tcp", proxy.listener.Addr().String()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	dbs, err := storage.OpenDatabases(context.Background(), config.DatabaseConfig{Primary: endpoint}, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbs.Close() })
	return dbs.Main, proxy
}

func TestMySQLHealthWriteProbeRecoversConnectionFailure(t *testing.T) {
	db, proxy := mysqlHealthProxyDatabase(t, false)
	h := &handler{nodes: nodes.NewService(db, nodes.Options{}), ready: true}
	lease, err := db.Acquire(context.Background(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	proxy.closeConnections()
	if _, err = tx.ExecContext(lease.Context(), "INSERT INTO settings(`key`,value) VALUES('connection_probe','uncommitted')"); err == nil || !storage.IsConnectionFailure(err) {
		t.Fatalf("expected a real connection failure, got %v", err)
	}
	_ = tx.Rollback()
	lease.Close()
	if db.Ready() {
		t.Fatal("broken connection left database ready")
	}
	proxy.offline.Store(true)
	if got := mysqlHealthRequest(h, "/healthz/ready"); got != http.StatusServiceUnavailable {
		t.Fatalf("unreachable database reported ready: %d", got)
	}
	if got := mysqlHealthRequest(h, "/healthz/live"); got != http.StatusOK {
		t.Fatalf("database failure changed liveness: %d", got)
	}
	proxy.offline.Store(false)
	if got := mysqlHealthRequest(h, "/healthz/ready"); got != http.StatusOK || !db.Ready() {
		t.Fatalf("guarded health probe failed to recover connection: status=%d ready=%v", got, db.Ready())
	}
	h.MarkNotReady()
	if got := mysqlHealthRequest(h, "/healthz/ready"); got != http.StatusServiceUnavailable {
		t.Fatalf("draining lifecycle reported ready: %d", got)
	}
	if got := mysqlHealthRequest(h, "/healthz/live"); got != http.StatusOK {
		t.Fatalf("draining lifecycle liveness=%d", got)
	}
}

func TestMySQLHealthWriteProbeResolvesBlackholedCommitReceipt(t *testing.T) {
	db, proxy := mysqlHealthProxyDatabase(t, true)
	h := &handler{nodes: nodes.NewService(db, nodes.Options{}), ready: true}
	lease, err := db.Acquire(context.Background(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(lease.Context(), "INSERT INTO settings(`key`,value) VALUES('commit_probe','0')"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(lease.Context(), "UPDATE settings SET value=CAST(value AS UNSIGNED)+1 WHERE `key`='commit_probe'"); err != nil {
		t.Fatal(err)
	}
	proxy.dropCommit.Store(true)
	started := time.Now()
	if err = tx.Commit(); !errors.Is(err, storage.ErrCommitUnknown) {
		t.Fatalf("blackholed COMMIT outcome=%v", err)
	}
	elapsed := time.Since(started)
	if elapsed < 4*time.Second || elapsed > 12*time.Second || proxy.dropped.Load() != 1 {
		t.Fatalf("COMMIT did not exit through the real driver read timeout: elapsed=%v dropped=%d", elapsed, proxy.dropped.Load())
	}
	lease.Close()
	if db.Ready() {
		t.Fatal("unknown COMMIT reported ready before guard resolution")
	}
	if got := mysqlHealthRequest(h, "/healthz/ready"); got != http.StatusOK || !db.Ready() {
		t.Fatalf("health probe did not resolve pending COMMIT: status=%d ready=%v", got, db.Ready())
	}
	committed, err := tx.ResolveCommit(context.Background())
	if err != nil || !committed {
		t.Fatalf("original receipt lost after health guard replaced its UUID: committed=%v err=%v", committed, err)
	}
	reader, err := db.Acquire(context.Background(), storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var count, value int
	var guard string
	if err = reader.QueryRowContext(reader.Context(), "SELECT count(*),MAX(CAST(value AS UNSIGNED)) FROM settings WHERE `key`='commit_probe'").Scan(&count, &value); err != nil || count != 1 || value != 1 {
		t.Fatalf("business mutation not committed exactly once: count=%d value=%d err=%v", count, value, err)
	}
	if err = reader.QueryRowContext(reader.Context(), "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1").Scan(&guard); err != nil || guard == tx.ID() {
		t.Fatalf("health probe did not commit a new guard receipt: err=%v", err)
	}
}

// 仅转发测试自建连接；丢弃一次 COMMIT 回包但不主动断开，让真实驱动读超时退出。
type mysqlHealthProxy struct {
	listener    net.Listener
	endpoint    config.DatabaseEndpoint
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closed      bool
	workers     sync.WaitGroup
	offline     atomic.Bool
	dropCommit  atomic.Bool
	dropped     atomic.Int32
}

func newMySQLHealthProxy(t *testing.T, endpoint config.DatabaseEndpoint) *mysqlHealthProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("mysql health proxy listener failed")
	}
	p := &mysqlHealthProxy{listener: listener, endpoint: endpoint, connections: make(map[net.Conn]struct{})}
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			if p.offline.Load() {
				_ = client.Close()
				continue
			}
			p.workers.Add(1)
			go p.forward(client)
		}
	}()
	t.Cleanup(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		_ = listener.Close()
		p.closeConnections()
		done := make(chan struct{})
		go func() { p.workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(7 * time.Second):
			t.Error("mysql health proxy did not stop within its dial timeout")
		}
	})
	return p
}

func (p *mysqlHealthProxy) closeConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for conn := range p.connections {
		_ = conn.Close()
	}
}

func (p *mysqlHealthProxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = conn.Close()
		return false
	}
	p.connections[conn] = struct{}{}
	return true
}

func (p *mysqlHealthProxy) forget(conn net.Conn) {
	_ = conn.Close()
	p.mu.Lock()
	delete(p.connections, conn)
	p.mu.Unlock()
}

func (p *mysqlHealthProxy) forward(client net.Conn) {
	defer p.workers.Done()
	if !p.track(client) {
		return
	}
	defer p.forget(client)
	network := p.endpoint.Network
	if network == "" {
		network = "tcp"
	}
	server, err := net.DialTimeout(network, p.endpoint.Address, 5*time.Second)
	if err != nil {
		return
	}
	if !p.track(server) {
		return
	}
	defer p.forget(server)
	if p.endpoint.TLS != nil {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = io.Copy(server, client)
			_ = server.Close()
		}()
		_, _ = io.Copy(client, server)
		_ = client.Close()
		_ = server.Close()
		<-done
		return
	}
	var dropResponse atomic.Bool
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		defer client.Close()
		defer server.Close()
		for {
			packet, err := mysqlHealthPacket(client)
			if err != nil {
				return
			}
			if len(packet) == 11 && packet[3] == 0 && packet[4] == 3 && bytes.EqualFold(packet[5:], []byte("COMMIT")) && p.dropCommit.CompareAndSwap(true, false) {
				dropResponse.Store(true)
			}
			if _, err = io.Copy(server, bytes.NewReader(packet)); err != nil {
				return
			}
		}
	}()
	for {
		packet, err := mysqlHealthPacket(server)
		if err != nil {
			break
		}
		if dropResponse.Swap(false) {
			p.dropped.Add(1)
			continue
		}
		if _, err = io.Copy(client, bytes.NewReader(packet)); err != nil {
			break
		}
	}
	_ = client.Close()
	_ = server.Close()
	<-clientDone
}

func mysqlHealthPacket(conn net.Conn) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	size := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	packet := make([]byte, 4+size)
	copy(packet, header[:])
	_, err := io.ReadFull(conn, packet[4:])
	return packet, err
}
