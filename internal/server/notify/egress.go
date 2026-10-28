package notify

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
)

var errEgress = errors.New("notification destination denied")

type resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}
type safeDialer struct {
	resolver     resolver
	allowPrivate bool
	dial         func(context.Context, string, string) (net.Conn, error)
}

func forbiddenIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	// Include CGNAT, reserved IPv4, IPv6 transition/mapped mechanisms and
	// documentation ranges: none are valid public notification destinations.
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

// Cloud instance metadata services stay unreachable even when private
// networks are explicitly allowed: they hand out host credentials.
func metadataIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	_, blocked := metadataAddrs[a]
	return blocked
}

// Instance metadata and container credential endpoints (AWS IMDS and ECS/EKS
// task credentials, Alibaba Cloud metadata).
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

// embeddedIPv4 returns IPv4 destinations encoded by transition mechanisms.
// RFC 6052 local-use NAT64 prefixes can have multiple lengths; check every
// possible encoding within 64:ff9b:1::/48, failing closed when
// an ambiguous encoding could reach metadata.
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
				} // reserved u octet
				v[i] = b[pos]
				pos++
			}
			out = append(out, net.IPv4(v[0], v[1], v[2], v[3]))
		}
		return out
	}
	return nil
}

func (d *safeDialer) denied(ip net.IP) bool {
	if metadataIP(ip) || !d.allowPrivate && forbiddenIP(ip) {
		return true
	}
	for _, embedded := range embeddedIPv4(ip) {
		if metadataIP(embedded) || !d.allowPrivate && forbiddenIP(embedded) {
			return true
		}
	}
	return false
}

func (d *safeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, errEgress
	}
	if strings.Contains(host, "%") {
		return nil, errEgress
	}
	ips, e := d.resolver.LookupIPAddr(ctx, host)
	if e != nil {
		return nil, e
	}
	if len(ips) == 0 {
		return nil, errEgress
	}
	// Reject mixed public/private answers too, before making any connection.
	for _, ip := range ips {
		if ip.Zone != "" || d.denied(ip.IP) {
			return nil, errEgress
		}
	}
	var last error
	for _, ip := range ips {
		// Dial the checked literal, never the hostname: no second DNS resolution.
		conn, e := d.dial(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if e == nil {
			return conn, nil
		}
		last = e
	}
	return nil, last
}
