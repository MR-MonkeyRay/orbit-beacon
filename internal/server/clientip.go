package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// 对丢弃端口的准入服务，以 host:port 形式保留地址。
func (h *handler) clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !h.trustedProxy(peer.WithZone("").Unmap()) {
		return r.RemoteAddr
	}
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return r.RemoteAddr
	}
	hops := strings.Split(strings.Join(values, ","), ",")
	addresses := make([]netip.Addr, len(hops))
	// 在让任何部分影响策略之前，验证整个链。
	for i, hop := range hops {
		address, err := netip.ParseAddr(strings.TrimSpace(hop))
		if err != nil || address.Zone() != "" {
			return r.RemoteAddr
		}
		addresses[i] = address.Unmap()
	}
	for i := len(addresses) - 1; i >= 0; i-- {
		if !h.trustedProxy(addresses[i]) {
			return net.JoinHostPort(addresses[i].String(), "0")
		}
	}
	return r.RemoteAddr
}

func (h *handler) trustedProxy(address netip.Addr) bool {
	for _, prefix := range h.trustedProxies {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
