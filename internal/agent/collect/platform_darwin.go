// SPDX-License-Identifier: GPL-3.0-or-later
//go:build darwin

package collect

import (
	"context"
	"net"
	"syscall"

	"github.com/shirou/gopsutil/v4/load"
)

func uptimeSourceAvailable() error { return nil }

func loadAverage(ctx context.Context) (loadValues, error) {
	value, err := load.AvgWithContext(ctx)
	if err != nil {
		return loadValues{}, err
	}
	return loadValues{one: value.Load1, five: value.Load5, fifteen: value.Load15}, nil
}

// Read the kernel routing table; do not execute route or guess the first NIC.
func defaultInterface(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := syscall.RouteRIB(syscall.NET_RT_DUMP, 0)
	if err != nil {
		return "", err
	}
	messages, err := syscall.ParseRoutingMessage(data)
	if err != nil {
		return "", err
	}
	for _, message := range messages {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		route, ok := message.(*syscall.RouteMessage)
		if !ok || route.Header.Flags&syscall.RTF_UP == 0 || route.Header.Flags&(syscall.RTF_REJECT|syscall.RTF_BLACKHOLE) != 0 {
			continue
		}
		addresses, err := syscall.ParseRoutingSockaddr(route)
		if err != nil || len(addresses) <= syscall.RTAX_NETMASK {
			continue
		}
		if !zeroRouteAddress(addresses[syscall.RTAX_DST]) || !zeroRouteAddress(addresses[syscall.RTAX_NETMASK]) {
			continue
		}
		iface, err := net.InterfaceByIndex(int(route.Header.Index))
		if err == nil && iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 {
			return iface.Name, nil
		}
	}
	return "", noDefaultInterface()
}

func zeroRouteAddress(address syscall.Sockaddr) bool {
	switch value := address.(type) {
	case *syscall.SockaddrInet4:
		return value.Addr == [4]byte{}
	case *syscall.SockaddrInet6:
		return value.Addr == [16]byte{}
	default:
		return false
	}
}
