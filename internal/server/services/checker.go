package services

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	"orbitbeacon/internal/server/egress"
)

type Checker interface {
	Check(context.Context, Service) Result
}
type NetworkChecker struct {
	dialer *egress.Dialer
	client *http.Client
}

func NewChecker() *NetworkChecker {
	d := &egress.Dialer{Resolver: net.DefaultResolver, AllowPrivate: true, Dial: (&net.Dialer{}).DialContext}
	transport := &http.Transport{DialContext: d.DialContext, DisableKeepAlives: true, Proxy: nil, ResponseHeaderTimeout: 30 * time.Second, MaxResponseHeaderBytes: 64 << 10}
	return &NetworkChecker{dialer: d, client: &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return errors.New("redirect limit")
		}
		if req.URL.User != nil || req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return egress.ErrDenied
		}
		return nil
	}}}
}
func errorCode(e error) string {
	if errors.Is(e, errUnsupported) {
		return "unsupported"
	}
	if errors.Is(e, egress.ErrDenied) {
		return "denied"
	}
	if errors.Is(e, context.DeadlineExceeded) {
		return "timeout"
	}
	var ne net.Error
	if errors.As(e, &ne) && ne.Timeout() {
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(e, &dns) {
		return "dns"
	}
	if errors.Is(e, syscall.ECONNREFUSED) {
		return "refused"
	}
	return "error"
}

var errUnsupported = errors.New("ICMP unsupported")
var errPermission = errors.New("ping socket permission denied")

func (c *NetworkChecker) Check(parent context.Context, s Service) Result {
	ctx, cancel := context.WithTimeout(parent, time.Duration(s.TimeoutSeconds)*time.Second)
	defer cancel()
	r := Result{CheckedAt: time.Now().UTC()}
	start := time.Now()
	var e error
	switch s.Type {
	case "http":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Target, nil)
		if err != nil {
			e = err
			break
		}
		req.Header.Set("User-Agent", "OrbitBeacon")
		response, err := c.client.Do(req)
		elapsed := float64(time.Since(start)) / float64(time.Millisecond)
		if response != nil {
			r.StatusCode = &response.StatusCode
			r.LatencyMS = &elapsed
			if response.TLS != nil && len(response.TLS.PeerCertificates) > 0 {
				at := response.TLS.PeerCertificates[0].NotAfter.UTC()
				r.TLSExpiresAt = &at
			}
			// Header latency is independent of body size or a streaming body.
			_, _ = io.CopyN(io.Discard, response.Body, 64<<10)
			response.Body.Close()
		}
		if err != nil {
			e = err
			break
		}
		r.OK = response.StatusCode >= 200 && response.StatusCode <= 399
		if !r.OK {
			r.Error = "http_status"
		}
	case "tcp":
		conn, err := c.dialer.DialContext(ctx, "tcp", s.Target)
		e = err
		if conn != nil {
			elapsed := float64(time.Since(start)) / float64(time.Millisecond)
			r.LatencyMS = &elapsed
			r.OK = true
			conn.Close()
		}
	case "icmp":
		r = c.ping(ctx, s.Target)
		r.CheckedAt = start.UTC()
	default:
		r.Error = "unsupported"
	}
	if e != nil {
		r.Error = errorCode(e)
	}
	return r
}
func (c *NetworkChecker) ping(ctx context.Context, target string) Result {
	if !pingSupported {
		return Result{Error: "unsupported"}
	}
	r := Result{}
	// egress validates every DNS answer and passes only a pinned IP to the socket factory.
	d := *c.dialer
	d.Dial = pingConn
	conn, e := d.DialContext(ctx, "udp", net.JoinHostPort(target, "0"))
	if e != nil {
		r.Error = errorCode(e)
		if errors.Is(e, errPermission) {
			r.Error = "permission_denied"
		}
		return r
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	var sum float64
	received := 0
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		r.Error = "error"
		return r
	}
	ipv6 := conn.RemoteAddr().(*net.UDPAddr).IP.To4() == nil
	for seq := 0; seq < 3; seq++ {
		if ctx.Err() != nil {
			break
		}
		packet := make([]byte, 24)
		packet[0] = 8
		if ipv6 {
			packet[0] = 128
		}
		binary.BigEndian.PutUint16(packet[6:8], uint16(seq+1))
		copy(packet[8:], nonce[:])
		if !ipv6 {
			binary.BigEndian.PutUint16(packet[2:4], checksum(packet))
		}
		// Each of the three packets gets an equal share of the original timeout.
		_ = conn.SetDeadline(minTime(deadline, time.Now().Add(remaining/3)))
		start := time.Now()
		if _, e = conn.Write(packet); e != nil {
			continue
		}
		buffer := make([]byte, 1500)
		for {
			n, err := conn.Read(buffer)
			if err != nil {
				break
			}
			typ := byte(0)
			if ipv6 {
				typ = 129
			}
			if n != len(packet) || buffer[0] != typ || buffer[1] != 0 || binary.BigEndian.Uint16(buffer[6:8]) != uint16(seq+1) {
				continue
			}
			same := true
			for i, v := range nonce {
				if buffer[8+i] != v {
					same = false
					break
				}
			}
			if !same {
				continue
			}
			received++
			sum += float64(time.Since(start)) / float64(time.Millisecond)
			break
		}
	}
	loss := float64(3-received) * 100 / 3
	r.PacketLoss = &loss
	r.OK = received > 0
	if received > 0 {
		avg := sum / float64(received)
		r.LatencyMS = &avg
	} else {
		r.Error = "timeout"
	}
	return r
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if len(b)%2 != 0 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
