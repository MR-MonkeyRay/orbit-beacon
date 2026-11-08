package storagetest

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/config"
)

// EndpointProxy 只中断本次夹具的 TCP 字节流，不解析 TLS、SQL 或凭据。
type EndpointProxy struct {
	listener         net.Listener
	network, address string
	mu               sync.Mutex
	links            map[net.Conn]net.Conn
	paused, closed   bool
	wg               sync.WaitGroup
}

func ProxyEndpoint(t testing.TB, endpoint config.DatabaseEndpoint) (*EndpointProxy, config.DatabaseEndpoint) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	network := endpoint.Network
	if network == "" {
		network = "tcp"
	}
	p := &EndpointProxy{listener: listener, network: network, address: endpoint.Address, links: map[net.Conn]net.Conn{}}
	if endpoint.TLS != nil {
		copy := *endpoint.TLS
		endpoint.TLS = &copy
		if copy.ServerName == "" {
			host, _, err := net.SplitHostPort(endpoint.Address)
			if err != nil {
				t.Fatal("invalid fixture TLS address")
			}
			endpoint.TLS.ServerName = host
		}
	}
	endpoint.Network = "tcp"
	endpoint.Address = listener.Addr().String()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			blocked := p.paused || p.closed
			if !blocked {
				p.wg.Add(1)
			}
			p.mu.Unlock()
			if blocked {
				client.Close()
				continue
			}
			go p.relay(client)
		}
	}()
	t.Cleanup(p.Close)
	return p, endpoint
}
func (p *EndpointProxy) relay(client net.Conn) {
	defer p.wg.Done()
	server, err := net.DialTimeout(p.network, p.address, 5*time.Second)
	if err != nil {
		client.Close()
		return
	}
	p.mu.Lock()
	if p.closed || p.paused {
		p.mu.Unlock()
		client.Close()
		server.Close()
		return
	}
	p.links[client] = server
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(server, client); client.Close(); server.Close() }()
	_, _ = io.Copy(client, server)
	client.Close()
	server.Close()
	<-done
	p.mu.Lock()
	delete(p.links, client)
	p.mu.Unlock()
}
func (p *EndpointProxy) Pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = true
	for client, server := range p.links {
		client.Close()
		server.Close()
	}
}
func (p *EndpointProxy) Resume() { p.mu.Lock(); p.paused = false; p.mu.Unlock() }
func (p *EndpointProxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.listener.Close()
	for client, server := range p.links {
		client.Close()
		server.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}
