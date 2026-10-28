//go:build linux

package probe

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var icmpSequence atomic.Uint32

func checkICMP(ctx context.Context, host string) (time.Duration, error) {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return 0, err
	}
	address, ipv6, err := preferredIP(addresses)
	if err != nil {
		return 0, err
	}

	family, protocol := unix.AF_INET, unix.IPPROTO_ICMP
	if ipv6 {
		family, protocol = unix.AF_INET6, unix.IPPROTO_ICMPV6
	}
	fd, err := unix.Socket(family, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, protocol)
	if err != nil {
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			return 0, icmpUnsupportedError{err}
		}
		return 0, err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.SetNonblock(fd, true); err != nil {
		return 0, err
	}

	deadline := time.Now().Add(defaultTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	sequence := uint16(icmpSequence.Add(1))
	packet := buildICMPEchoRequest(ipv6, sequence)
	started := time.Now()
	if err := unix.Sendto(fd, packet, 0, sockaddr(address, ipv6)); err != nil {
		return 0, normalizeICMPTimeout(ctx, err)
	}
	buffer := make([]byte, 1500)
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, context.DeadlineExceeded
		}
		wait := min(remaining, 50*time.Millisecond)
		pollfds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		ready, err := unix.Poll(pollfds, int(wait.Milliseconds()+1))
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return 0, err
		}
		if ready == 0 {
			continue
		}
		count, _, err := unix.Recvfrom(fd, buffer, unix.MSG_DONTWAIT)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				continue
			}
			return 0, normalizeICMPTimeout(ctx, err)
		}
		if parseICMPEchoReply(buffer[:count], ipv6, sequence) == nil {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			return time.Since(started), nil
		}
	}
}

func preferredIP(addresses []net.IPAddr) (net.IP, bool, error) {
	for _, address := range addresses {
		if ipv4 := address.IP.To4(); ipv4 != nil {
			return ipv4, false, nil
		}
	}
	for _, address := range addresses {
		if ipv6 := address.IP.To16(); ipv6 != nil {
			return ipv6, true, nil
		}
	}
	return nil, false, &net.DNSError{Err: "no suitable IP address", IsNotFound: true}
}

func sockaddr(address net.IP, ipv6 bool) unix.Sockaddr {
	if ipv6 {
		value := &unix.SockaddrInet6{}
		copy(value.Addr[:], address.To16())
		return value
	}
	value := &unix.SockaddrInet4{}
	copy(value.Addr[:], address.To4())
	return value
}

func normalizeICMPTimeout(ctx context.Context, err error) error {
	if contextError := ctx.Err(); contextError != nil {
		return contextError
	}
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
		return context.DeadlineExceeded
	}
	return err
}
