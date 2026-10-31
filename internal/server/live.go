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

// liveFrame 是通用的只读浏览器传输契约。数据仅存在于快照帧中；
// 控制帧绝不携带页面，也不伪造采样时间。
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

// liveStreams 管理准入和所有只读流，包括已由 Hijack 接管、net/http.Shutdown 无法管理的
// 套接字。每个租约只释放一次容量。
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
	// 传输回调在该租约自己的互斥锁下运行，绝不在准入互斥锁下运行。
	// 这样截止时间设置、中断和租约释放都被串行化。
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

// CloseLiveStreams 关闭准入、取消查询并中断活跃写入。
// 空闲流可以在关闭套接字之前发送关闭帧。
func (h *handler) CloseLiveStreams() {
	for _, stream := range h.streams.snapshot(true) {
		stream.stop(false)
	}
}

// WaitLiveStreams 是退出屏障，而不仅是取消通知。
// 超时会主动关闭套接字；出错时调用方不得排空 ingest。
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

// publicPagesEqual 忽略投影和传输时间，直接比较字段，
// 避免仅为判断是否发送心跳就序列化未变化的 100 节点页面。
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

// liveSource 为共享的只读发送器提供投影、内容比较和可选的私有数据认证。
// 具体传输策略由外层处理。
type liveSource[T any] struct {
	snapshot     func(context.Context) (T, uint64, error)
	equal        func(T, T) bool
	authenticate func(context.Context) error
}

// 只有这些哨兵值是终止协议错误。数据库和传输故障绝不能伪装成策略决定或会话过期。
func liveTerminalCode(err error) string {
	switch {
	case errors.Is(err, errPublicPageDisabled):
		return "public_page_disabled"
	case errors.Is(err, auth.ErrUnauthenticated):
		return "unauthenticated"
	default:
		return ""
	}
}

func (h *handler) runPublicLive(ctx context.Context, request context.Context, stream *liveStream, limit, offset int, send func(context.Context, []byte) error) {
	runLive(h, ctx, request, stream, liveSource[ingest.PublicPage]{
		snapshot: func(ctx context.Context) (ingest.PublicPage, uint64, error) {
			return h.publicSnapshotVersion(ctx, limit, offset)
		},
		equal:        publicPagesEqual,
		authenticate: func(context.Context) error { return h.publicPageAccess() },
	}, send)
}

func writeLiveFrame[T any](h *handler, ctx context.Context, stream *liveStream, authenticate func(context.Context) error, send func(context.Context, []byte) error, projectionVersion uint64, frame liveFrame[T], terminal bool) error {
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
		if !terminal && authenticate != nil {
			if err := authenticate(writeCtx); err != nil {
				return err
			}
		}
		if frame.Type == "snapshot" && h.nodes.PublicVisibilityVersion() != projectionVersion {
			return errSnapshotBusy
		}
		return send(writeCtx, body)
	})
}

func liveTerminalState(ctx context.Context, authenticate func(context.Context) error, err error) (string, error) {
	if code := liveTerminalCode(err); code != "" {
		return code, err
	}
	if code := liveTerminalCode(context.Cause(ctx)); code != "" {
		return code, context.Cause(ctx)
	}
	if authenticate != nil {
		// 安全撤回可能已取消了投影/写入上下文。
		// 独立地重新检查策略，使取消无法掩盖其真实原因。
		checkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = authenticate(checkCtx)
		cancel()
		return liveTerminalCode(err), err
	}
	return "", nil
}

func liveTerminalError[T any](h *handler, ctx context.Context, authenticate func(context.Context) error, write func(liveFrame[T], bool) error, err error) bool {
	code, _ := liveTerminalState(ctx, authenticate, err)
	if code == "" {
		return false
	}
	_ = write(controlFrame[T](h, "error", code), true)
	return true
}

func closeLiveFrame[T any](h *handler, ctx context.Context, authenticate func(context.Context) error, write func(liveFrame[T], bool) error, code string) {
	terminal, err := liveTerminalState(ctx, authenticate, nil)
	if terminal != "" {
		_ = write(controlFrame[T](h, "error", terminal), true)
		return
	}
	if err == nil {
		_ = write(controlFrame[T](h, "close", code), true)
	}
}

// 先订阅再执行首次查询：身份/可见性撤回不能落在首次投影与等待通知之间的间隙里。
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
		return writeLiveFrame(h, ctx, stream, source.authenticate, send, projectionVersion, frame, terminal)
	}
	terminalError := func(err error) bool { return liveTerminalError(h, ctx, source.authenticate, write, err) }
	closeFrame := func(code string) { closeLiveFrame(h, ctx, source.authenticate, write, code) }
	for {
		if ctx.Err() != nil {
			if terminalError(context.Cause(ctx)) {
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
					if terminalError(err) {
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
				if terminalError(err) {
					return
				}
				if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					continue
				}
				return
			}
			// 发送数据前再次检查，而不只在缓存前检查；已知失效的页面不能通过任何传输方式发出。
			if h.nodes.PublicSafetyVersion() != visibility {
				closeFrame("renew")
				return
			}
			projectionVersion = version
			nextProjection = time.Now().Add(interval)
			if first || !source.equal(previous, page) {
				if err := write(snapshotFrame(h, page), false); err != nil {
					if terminalError(err) {
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
				terminalError(err)
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
