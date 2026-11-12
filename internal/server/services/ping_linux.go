//go:build linux

package services

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
)

const pingSupported = true

// Linux ping sockets are unprivileged datagram ICMP, not UDP. net.ListenPacket
// ("udp4") would select IPPROTO_UDP; FilePacketConn adopts the correct socket.
func pingConn(ctx context.Context, _, address string) (net.Conn, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	host, _, e := net.SplitHostPort(address)
	if e != nil {
		return nil, e
	}
	ip := net.ParseIP(host)
	family, protocol := syscall.AF_INET, syscall.IPPROTO_ICMP
	if ip.To4() == nil {
		family, protocol = syscall.AF_INET6, syscall.IPPROTO_ICMPV6
	}
	fd, e := syscall.Socket(family, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, protocol)
	if e != nil {
		return nil, pingSocketError(e)
	}
	file := os.NewFile(uintptr(fd), "orbitbeacon-ping")
	defer file.Close()
	packet, e := net.FilePacketConn(file)
	if e != nil {
		return nil, e
	}
	return &pingConnection{PacketConn: packet, remote: &net.UDPAddr{IP: ip}}, nil
}

type pingConnection struct {
	net.PacketConn
	remote *net.UDPAddr
}

func (p *pingConnection) Read(b []byte) (int, error) {
	for {
		n, from, e := p.ReadFrom(b)
		if e != nil {
			return n, e
		}
		if u, ok := from.(*net.UDPAddr); ok && u.IP.Equal(p.remote.IP) {
			return n, nil
		}
	}
}
func (p *pingConnection) Write(b []byte) (int, error) { return p.WriteTo(b, p.remote) }
func (p *pingConnection) RemoteAddr() net.Addr        { return p.remote }

// Permission hints apply only to failure to open a Linux ping socket.
func pingSocketError(e error) error {
	if errors.Is(e, syscall.EACCES) || errors.Is(e, syscall.EPERM) {
		return errPermission
	}
	return e
}
