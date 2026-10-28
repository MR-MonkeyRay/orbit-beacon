package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"orbitbeacon/internal/server/nodes"
)

type publicStreams struct {
	mu      sync.Mutex
	total   int
	byIP    map[string]int
	ctx     context.Context
	cancel  context.CancelFunc
	closing bool
}

func (s *publicStreams) acquire(ip string) (context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.total >= 200 || s.byIP[ip] >= 4 {
		return nil, false
	}
	if s.ctx == nil {
		s.ctx, s.cancel = context.WithCancel(context.Background())
		s.byIP = make(map[string]int)
	}
	s.total++
	s.byIP[ip]++
	return s.ctx, true
}

func (s *publicStreams) release(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total--
	s.byIP[ip]--
	if s.byIP[ip] == 0 {
		delete(s.byIP, ip)
	}
}

// ClosePublicStreams wakes active streams and rejects new ones during shutdown.
func (h *handler) ClosePublicStreams() {
	h.streams.mu.Lock()
	defer h.streams.mu.Unlock()
	h.streams.closing = true
	if h.streams.cancel != nil {
		h.streams.cancel()
	}
}

func (h *handler) publicStreamIP(r *http.Request) string {
	address := h.clientAddr(r)
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	if ip, err := netip.ParseAddr(address); err == nil {
		return ip.WithZone("").Unmap().String()
	}
	return address
}

func streamDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (h *handler) publicStream(w http.ResponseWriter, r *http.Request, requestID string) {
	limit, offset, err := pagination(r)
	if err != nil {
		h.serviceError(w, nodes.ErrInvalidInput, requestID)
		return
	}
	ip := h.publicStreamIP(r)
	shutdown, ok := h.streams.acquire(ip)
	if !ok {
		w.Header().Set("Retry-After", "10")
		fail(w, http.StatusServiceUnavailable, "stream_busy", "Public stream capacity is unavailable", requestID)
		return
	}
	defer h.streams.release(ip)
	if h.metrics != nil {
		h.metrics.publicStreams.Add(1)
		defer h.metrics.publicStreams.Add(-1)
	}
	ctx, cancel := context.WithTimeout(r.Context(), streamDuration(h.streamLifetime, 15*time.Minute))
	defer cancel()
	stop := context.AfterFunc(shutdown, cancel)
	defer stop()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	interval := streamDuration(h.streamInterval, 2*time.Second)
	heartbeat := streamDuration(h.streamHeartbeat, 10*time.Second)
	var sentNodes, sentReports uint64
	var last time.Time
	requeries := 0
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			if r.Context().Err() == nil {
				_ = writePublicFrame(w, "close", []byte("{}"))
			}
			return
		}
		versionNodes, changedNodes := h.nodes.PublicChanges()
		versionReports, changedReports := h.ingest.PublicChanges()
		due := last.Add(heartbeat)
		if versionNodes != sentNodes || versionReports != sentReports {
			due = last.Add(min(interval, heartbeat))
		}
		if last.IsZero() || !time.Now().Before(due) {
			page, err := h.ingest.PublicPage(ctx, h.nodes, limit, offset, h.now)
			if err != nil {
				if cause := ctx.Err(); cause != nil && errors.Is(err, cause) && r.Context().Err() == nil {
					_ = writePublicFrame(w, "close", []byte("{}"))
				}
				return
			}
			// A visibility change during the query may have hidden a node in this
			// page; requery so unpublishing never leaks into a later frame.
			if current, _ := h.nodes.PublicChanges(); current != versionNodes {
				// Never emit a known-stale page; persistent churn ends the stream
				// without close, so the client clears its data and retries.
				if requeries >= 3 {
					return
				}
				requeries++
				continue
			}
			requeries = 0
			body, err := json.Marshal(page)
			if err != nil || writePublicFrame(w, "nodes", body) != nil {
				return
			}
			last = time.Now()
			// Changes during the query remain pending for the next frame.
			sentNodes, sentReports = versionNodes, versionReports
			continue
		}
		timer.Reset(time.Until(due))
		select {
		case <-ctx.Done():
		case <-changedNodes:
		case <-changedReports:
		case <-timer.C:
		}
	}
}

func writePublicFrame(w http.ResponseWriter, event string, body []byte) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	frame := append([]byte("event: "+event+"\ndata: "), body...)
	frame = append(frame, '\n', '\n')
	n, err := w.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	if err := controller.Flush(); err != nil {
		return err
	}
	// HTTP/2 resets a stream when its deadline fires even while idle, so the
	// deadline covers only this write; the next frame arms a fresh one.
	return controller.SetWriteDeadline(time.Time{})
}
