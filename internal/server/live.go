package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/ingest"
)

// liveFrame is the common read-only browser wire contract. Data is present only
// for snapshots; control frames never include a page or fabricate sample time.
type liveFrame[T any] struct {
	Type                  string    `json:"type"`
	ServerTime            time.Time `json:"server_time"`
	UpdateIntervalSeconds int       `json:"update_interval_seconds"`
	Data                  *T        `json:"data,omitempty"`
	Code                  string    `json:"code,omitempty"`
}

func (h *handler) liveSeconds() int {
	if h.webUpdateIntervalSeconds == 0 {
		return 10
	}
	return h.webUpdateIntervalSeconds
}

func streamDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (h *handler) liveInterval() time.Duration {
	return streamDuration(h.streamInterval, time.Duration(h.liveSeconds())*time.Second)
}

func snapshotFrame[T any](h *handler, data T) liveFrame[T] {
	return liveFrame[T]{Type: "snapshot", ServerTime: h.now().UTC(), UpdateIntervalSeconds: h.liveSeconds(), Data: &data}
}

func controlFrame[T any](h *handler, kind, code string) liveFrame[T] {
	return liveFrame[T]{Type: kind, ServerTime: h.now().UTC(), UpdateIntervalSeconds: h.liveSeconds(), Code: code}
}

// liveStreams owns admission and every read-only stream, including hijacked
// sockets which net/http's Shutdown cannot see. A lease releases capacity once.
type liveStreams struct {
	mu      sync.Mutex
	total   int
	byIP    map[string]int
	active  map[*liveStream]struct{}
	done    chan struct{}
	closing bool
}

type liveStream struct {
	registry  *liveStreams
	ip        string
	ctx       context.Context
	cancel    context.CancelFunc
	once      sync.Once
	mu        sync.Mutex
	writing   bool
	terminal  bool
	interrupt func()
	prepare   func() error
	forced    bool
}

func (s *liveStreams) acquire(ip string) (*liveStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.total >= 200 || s.byIP[ip] >= 4 {
		return nil, false
	}
	if s.byIP == nil {
		s.byIP = make(map[string]int)
		s.active = make(map[*liveStream]struct{})
	}
	if s.total == 0 {
		s.done = make(chan struct{})
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &liveStream{registry: s, ip: ip, ctx: ctx, cancel: cancel}
	s.active[stream] = struct{}{}
	s.total++
	s.byIP[ip]++
	return stream, true
}

func (s *liveStream) release() {
	s.once.Do(func() {
		s.mu.Lock()
		s.cancel()
		s.forced = true
		s.interrupt = nil
		s.prepare = nil
		s.mu.Unlock()
		r := s.registry
		r.mu.Lock()
		delete(r.active, s)
		r.total--
		r.byIP[s.ip]--
		if r.byIP[s.ip] == 0 {
			delete(r.byIP, s.ip)
		}
		if r.total == 0 {
			close(r.done)
		}
		r.mu.Unlock()
	})
}

func (s *liveStream) setWriter(prepare func() error, interrupt func()) {
	s.mu.Lock()
	s.prepare = prepare
	s.interrupt = interrupt
	if s.ctx.Err() != nil {
		interrupt()
	}
	s.mu.Unlock()
}

func (s *liveStream) stop(force bool) {
	s.mu.Lock()
	s.cancel()
	// Transport callbacks run under this lease's mutex, never the admission
	// mutex. This serializes deadline arming, interruption and lease release.
	if force {
		s.forced = true
	}
	if s.interrupt != nil && (force || s.writing) {
		s.interrupt()
	}
	s.mu.Unlock()
}

func (s *liveStream) interruptDataWrite() {
	s.mu.Lock()
	if s.writing && !s.terminal && s.interrupt != nil {
		s.interrupt()
	}
	s.mu.Unlock()
}

func (s *liveStream) write(ctx context.Context, terminal bool, write func() error) error {
	s.mu.Lock()
	if s.forced || ctx.Err() != nil || !terminal && s.ctx.Err() != nil {
		s.mu.Unlock()
		return context.Canceled
	}
	if s.prepare != nil {
		if err := s.prepare(); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.writing = true
	s.terminal = terminal
	s.mu.Unlock()
	err := write()
	s.mu.Lock()
	s.writing = false
	s.mu.Unlock()
	return err
}

func (s *liveStreams) snapshot(closeAdmission bool) []*liveStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	if closeAdmission {
		s.closing = true
	}
	streams := make([]*liveStream, 0, len(s.active))
	for stream := range s.active {
		streams = append(streams, stream)
	}
	return streams
}

// CloseLiveStreams closes admission, cancels queries and interrupts active
// writes. Idle streams can send a shutdown frame before closing their socket.
func (h *handler) CloseLiveStreams() {
	for _, stream := range h.streams.snapshot(true) {
		stream.stop(false)
	}
}

// WaitLiveStreams is an exit barrier, not just a cancellation notification. A
// deadline actively closes sockets; the caller must not drain ingest on error.
func (h *handler) WaitLiveStreams(ctx context.Context) error {
	h.streams.mu.Lock()
	if h.streams.total == 0 {
		h.streams.mu.Unlock()
		return nil
	}
	done := h.streams.done
	h.streams.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		for _, stream := range h.streams.snapshot(false) {
			stream.stop(true)
		}
		return ctx.Err()
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

// publicPagesEqual ignores projection/transport clocks. Its typed comparisons
// avoid marshaling an unchanged 100-node page just to discover a heartbeat.
func publicPagesEqual(a, b ingest.PublicPage) bool {
	if a.HasMore != b.HasMore || !equalLiveValue(a.NextOffset, b.NextOffset) {
		return false
	}
	return slices.EqualFunc(a.Nodes, b.Nodes, func(a, b ingest.PublicNode) bool {
		return a.Name == b.Name && a.Region == b.Region && a.Provider == b.Provider &&
			a.Status == b.Status && equalLiveTime(a.LastSeen, b.LastSeen) &&
			a.MetricStatus == b.MetricStatus && equalLiveValue(a.CPUPercent, b.CPUPercent) &&
			equalLiveValue(a.MemoryPercent, b.MemoryPercent) && equalLiveValue(a.Load1, b.Load1) &&
			equalLiveValue(a.UptimeSeconds, b.UptimeSeconds)
	})
}

func equalLiveValue[T comparable](a, b *T) bool {
	return a == b || a != nil && b != nil && *a == *b
}

func equalLiveTime(a, b *time.Time) bool {
	return a == b || a != nil && b != nil && a.Equal(*b)
}

// liveSource keeps only projection, semantic equality and optional private
// authentication in the shared read-only sender. Transport policy stays here.
type liveSource[T any] struct {
	snapshot     func(context.Context) (T, uint64, error)
	equal        func(T, T) bool
	authenticate func(context.Context) error
}

func (h *handler) runPublicLive(ctx context.Context, request context.Context, stream *liveStream, limit, offset int, send func(context.Context, []byte) error) {
	runLive(h, ctx, request, stream, liveSource[ingest.PublicPage]{
		snapshot: func(ctx context.Context) (ingest.PublicPage, uint64, error) {
			return h.publicSnapshotVersion(ctx, limit, offset)
		},
		equal: publicPagesEqual,
	}, send)
}

// Subscribe before the first query: identity/visibility withdrawal cannot fall
// in the gap between the initial projection and waiting for notifications.
func runLive[T any](h *handler, ctx context.Context, request context.Context, stream *liveStream, source liveSource[T], send func(context.Context, []byte) error) {
	visibility, visibilityChanged := h.nodes.PublicSafetyChanges()
	ctx, cancel := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	defer func() {
		cancel()
		<-watchDone
	}()
	go func() {
		defer close(watchDone)
		select {
		case <-visibilityChanged:
			cancel()
			stream.interruptDataWrite()
		case <-ctx.Done():
		}
	}()
	interval := h.liveInterval()
	heartbeat := streamDuration(h.streamHeartbeat, 10*time.Second)
	var previous T
	var projectionVersion uint64
	var nextProjection, nextHeartbeat time.Time
	first := true
	timer := time.NewTimer(0)
	defer timer.Stop()
	write := func(frame liveFrame[T], terminal bool) error {
		body, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		if frame.Type == "snapshot" && h.nodes.PublicVisibilityVersion() != projectionVersion {
			return errSnapshotBusy
		}
		writeCtx := ctx
		if terminal {
			writeCtx = context.Background()
		}
		writeCtx, cancel := context.WithTimeout(writeCtx, 10*time.Second)
		defer cancel()
		return stream.write(writeCtx, terminal, func() error {
			if !terminal && source.authenticate != nil {
				if err := source.authenticate(writeCtx); err != nil {
					return err
				}
			}
			if frame.Type == "snapshot" && h.nodes.PublicVisibilityVersion() != projectionVersion {
				return errSnapshotBusy
			}
			return send(writeCtx, body)
		})
	}
	closeFrame := func(code string) {
		if source.authenticate != nil {
			checkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := source.authenticate(checkCtx)
			cancel()
			if errors.Is(err, auth.ErrUnauthenticated) || errors.Is(context.Cause(ctx), auth.ErrUnauthenticated) {
				_ = write(controlFrame[T](h, "error", "unauthenticated"), true)
				return
			}
			if err != nil {
				return
			}
		}
		_ = write(controlFrame[T](h, "close", code), true)
	}
	unauthorized := func() { _ = write(controlFrame[T](h, "error", "unauthenticated"), true) }
	isUnauthorized := func(err error) bool {
		return errors.Is(err, auth.ErrUnauthenticated) || errors.Is(context.Cause(ctx), auth.ErrUnauthenticated)
	}
	for {
		if ctx.Err() != nil {
			if errors.Is(context.Cause(ctx), auth.ErrUnauthenticated) {
				unauthorized()
				return
			}
			if request.Err() == nil {
				code := "renew"
				if stream.ctx.Err() != nil {
					code = "shutdown"
				}
				closeFrame(code)
			}
			return
		}
		if !first && h.nodes.PublicSafetyVersion() != visibility {
			closeFrame("renew")
			return
		}
		now := time.Now()
		if first || !now.Before(nextProjection) {
			if source.authenticate != nil {
				if err := source.authenticate(ctx); err != nil {
					if isUnauthorized(err) {
						unauthorized()
						return
					}
					if ctx.Err() != nil {
						continue
					}
					return
				}
			}
			page, version, err := source.snapshot(ctx)
			if err != nil {
				if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					continue
				}
				return
			}
			// Check again immediately before outbound data, not merely before
			// caching it. A known obsolete page never reaches either transport.
			if h.nodes.PublicSafetyVersion() != visibility {
				closeFrame("renew")
				return
			}
			projectionVersion = version
			nextProjection = time.Now().Add(interval)
			if first || !source.equal(previous, page) {
				if err := write(snapshotFrame(h, page), false); err != nil {
					if isUnauthorized(err) {
						unauthorized()
						return
					}
					if errors.Is(err, errSnapshotBusy) && h.nodes.PublicSafetyVersion() != visibility {
						closeFrame("renew")
					}
					return
				}
				previous = page
				nextHeartbeat = time.Now().Add(heartbeat)
			}
			first = false
		}
		if !time.Now().Before(nextHeartbeat) {
			if err := write(controlFrame[T](h, "heartbeat", ""), false); err != nil {
				if isUnauthorized(err) {
					unauthorized()
				}
				return
			}
			nextHeartbeat = time.Now().Add(heartbeat)
		}
		due := nextProjection
		if nextHeartbeat.Before(due) {
			due = nextHeartbeat
		}
		timer.Reset(time.Until(due))
		select {
		case <-ctx.Done():
		case <-visibilityChanged:
			closeFrame("renew")
			return
		case <-timer.C:
		}
	}
}
