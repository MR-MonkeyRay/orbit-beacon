package storage_test

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"orbitbeacon/internal/config"
)

type replicaProxyLink struct {
	client, server net.Conn
	drop           atomic.Bool
}
type replicaProxy struct {
	listener net.Listener
	mu       sync.Mutex
	links    map[*replicaProxyLink]bool
	closed   bool
	wg       sync.WaitGroup
	dropped  atomic.Int64
}

// 仅代理隔离夹具连接，不解析、记录或改写 SQL/TLS/凭据。
func newReplicaProxy(t *testing.T, endpoint config.DatabaseEndpoint) (*replicaProxy, config.DatabaseEndpoint) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &replicaProxy{listener: listener, links: map[*replicaProxyLink]bool{}}
	network := endpoint.Network
	if network == "" {
		network = "tcp"
	}
	address := endpoint.Address
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			p.wg.Add(1)
			go p.relay(client, network, address)
		}
	}()
	t.Cleanup(p.close)
	if endpoint.TLS != nil {
		copy := *endpoint.TLS
		endpoint.TLS = &copy
		if copy.ServerName == "" {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				t.Fatal("invalid fixture TLS endpoint")
			}
			endpoint.TLS.ServerName = host
		}
	}
	endpoint.Network = "tcp"
	endpoint.Address = listener.Addr().String()
	return p, endpoint
}
func (p *replicaProxy) relay(client net.Conn, network, address string) {
	defer p.wg.Done()
	server, err := net.DialTimeout(network, address, 5*time.Second)
	if err != nil {
		client.Close()
		return
	}
	link := &replicaProxyLink{client: client, server: server}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		client.Close()
		server.Close()
		return
	}
	p.links[link] = true
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(server, client); server.Close(); client.Close() }()
	defer func() {
		server.Close()
		client.Close()
		<-done
		p.mu.Lock()
		delete(p.links, link)
		p.mu.Unlock()
	}()
	buffer := make([]byte, 32<<10)
	defer clear(buffer)
	for {
		n, err := server.Read(buffer)
		if n > 0 {
			if link.drop.Load() {
				p.dropped.Add(int64(n))
			} else {
				at := 0
				for at < n {
					written, e := client.Write(buffer[at:n])
					if e != nil || written == 0 {
						return
					}
					at += written
				}
			}
		}
		if err != nil {
			return
		}
	}
}
func (p *replicaProxy) blackhole() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for link := range p.links {
		link.drop.Store(true)
	}
}
func (p *replicaProxy) resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for link := range p.links {
		link.drop.Store(false)
	}
}
func (p *replicaProxy) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.listener.Close()
	for link := range p.links {
		link.client.Close()
		link.server.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}
