// Package egress 校验目标地址，并将连接固定到已检查的 IP。
package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
)

var ErrDenied = errors.New("destination denied by egress policy")

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// Dialer 解析并检查每个应答，仅拨号已通过检查的 IP 字面量。
// 调用方须设置 Resolver 和 Dial。AllowPrivate 保留通知渠道的显式放行，
// 包括回环地址；DenyLocal 在此基础上额外阻止本地/非单播目标（备份用）。
// 元数据端点始终拒绝，包括嵌入的 IPv4 目标。
type Dialer struct {
	Resolver     Resolver
	Dial         func(context.Context, string, string) (net.Conn, error)
	AllowPrivate bool
	DenyLocal    bool
}

func forbiddenIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	// 包括 CGNAT、保留 IPv4、IPv6 过渡/映射机制及文档地址段：均非合法公网目标。
	if !a.IsGlobalUnicast() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// 云实例元数据服务即便在显式允许私网时也不可达：它们会泄露主机凭据。
func metadataIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	_, blocked := metadataAddrs[a]
	return blocked
}

// 实例元数据与容器凭据端点（AWS IMDS 及 ECS/EKS 任务凭据、阿里云元数据）。
var metadataAddrs = map[netip.Addr]struct{}{
	netip.MustParseAddr("169.254.169.254"): {}, netip.MustParseAddr("fd00:ec2::254"): {},
	netip.MustParseAddr("169.254.170.2"): {}, netip.MustParseAddr("169.254.170.23"): {},
	netip.MustParseAddr("fd00:ec2::23"): {}, netip.MustParseAddr("100.100.100.200"): {},
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("::/96"), netip.MustParsePrefix("::ffff:0:0:0/96"),
}

// embeddedIPv4 返回经过渡机制编码的 IPv4 目标。
// RFC 6052 本地 NAT64 前缀可有多种长度；须检查 64:ff9b:1::/48 内所有可能编码，
// 模糊编码可能触及元数据时一律拒绝。
func embeddedIPv4(ip net.IP) []net.IP {
	a, ok := netip.AddrFromSlice(ip)
	if !ok || a.Is4() {
		return nil
	}
	b := a.As16()
	switch {
	case a.Is4In6(), netip.MustParsePrefix("::/96").Contains(a), netip.MustParsePrefix("::ffff:0:0:0/96").Contains(a), netip.MustParsePrefix("64:ff9b::/96").Contains(a):
		return []net.IP{net.IPv4(b[12], b[13], b[14], b[15])}
	case netip.MustParsePrefix("2002::/16").Contains(a):
		return []net.IP{net.IPv4(b[2], b[3], b[4], b[5])}
	case netip.MustParsePrefix("2001::/32").Contains(a):
		return []net.IP{net.IPv4(^b[12], ^b[13], ^b[14], ^b[15])}
	case netip.MustParsePrefix("64:ff9b:1::/48").Contains(a):
		out := []net.IP{net.IPv4(b[12], b[13], b[14], b[15])} // /96
		if b[8] != 0 {
			return out
		}
		for _, start := range []int{6, 7, 8} { // /48, /56, /64
			var v [4]byte
			pos := start
			for i := range v {
				if pos == 8 {
					pos++
				} // 保留的 u 八位组
				v[i] = b[pos]
				pos++
			}
			out = append(out, net.IPv4(v[0], v[1], v[2], v[3]))
		}
		return out
	}
	return nil
}

func localIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	return !a.IsGlobalUnicast() || a.IsLoopback() || a.IsLinkLocalUnicast()
}

func (d *Dialer) denied(ip net.IP) bool {
	if metadataIP(ip) || !d.AllowPrivate && forbiddenIP(ip) || d.DenyLocal && localIP(ip) {
		return true
	}
	for _, embedded := range embeddedIPv4(ip) {
		if metadataIP(embedded) || !d.AllowPrivate && forbiddenIP(embedded) || d.DenyLocal && localIP(embedded) {
			return true
		}
	}
	return false
}

func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, ErrDenied
	}
	if strings.Contains(host, "%") {
		return nil, ErrDenied
	}
	ips, e := d.Resolver.LookupIPAddr(ctx, host)
	if e != nil {
		return nil, e
	}
	if len(ips) == 0 {
		return nil, ErrDenied
	}
	// 建立任何连接前，也拒绝公网/私网混合的应答。
	for _, ip := range ips {
		if ip.Zone != "" || d.denied(ip.IP) {
			return nil, ErrDenied
		}
	}
	var last error
	for _, ip := range ips {
		// 拨号已检查的字面量，绝不拨主机名：避免二次 DNS 解析。
		conn, e := d.Dial(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if e == nil {
			return conn, nil
		}
		last = e
	}
	return nil, last
}
