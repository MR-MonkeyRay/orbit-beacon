package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"orbitbeacon/internal/server/nodes"
)

// Capture only the accepted socket so the HTTP handshake deadline can be
// cleared after the registry takes ownership. RFC 6455 remains the library's.
type liveUpgradeWriter struct {
	http.ResponseWriter
	connection net.Conn
	stream     *liveStream
}

func (w *liveUpgradeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *liveUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	connection, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.connection = connection
		w.stream.setWriter(nil, func() { _ = connection.Close() })
	}
	return connection, rw, err
}

func (h *handler) admitPublicStream(w http.ResponseWriter, r *http.Request, requestID string) (*liveStream, int, int, bool) {
	limit, offset, err := pagination(r)
	if err != nil {
		h.serviceError(w, nodes.ErrInvalidInput, requestID)
		return nil, 0, 0, false
	}
	stream, ok := h.streams.acquire(h.publicStreamIP(r))
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(h.liveSeconds()))
		fail(w, http.StatusServiceUnavailable, "stream_busy", "Public stream capacity is unavailable", requestID)
		return nil, 0, 0, false
	}
	if h.metrics != nil {
		h.metrics.publicStreams.Add(1)
	}
	return stream, limit, offset, true
}

func (h *handler) releasePublicStream(stream *liveStream) {
	if h.metrics != nil {
		h.metrics.publicStreams.Add(-1)
	}
	stream.release()
}

func (h *handler) publicStreamContext(r *http.Request, stream *liveStream) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(stream.ctx, streamDuration(h.streamLifetime, 15*time.Minute))
	stop := context.AfterFunc(r.Context(), cancel)
	return ctx, func() { stop(); cancel() }
}

func (h *handler) publicStream(w http.ResponseWriter, r *http.Request, requestID string) {
	stream, limit, offset, ok := h.admitPublicStream(w, r, requestID)
	if !ok {
		return
	}
	defer h.releasePublicStream(stream)
	ctx, cancel := h.publicStreamContext(r, stream)
	defer cancel()
	controller := http.NewResponseController(w)
	stream.setWriter(func() error { return controller.SetWriteDeadline(time.Now().Add(10 * time.Second)) }, func() { _ = controller.SetWriteDeadline(time.Now()) })
	writeInterrupted := make(chan struct{})
	stopWrite := context.AfterFunc(ctx, func() { stream.interruptDataWrite(); close(writeInterrupted) })
	defer func() {
		if !stopWrite() {
			<-writeInterrupted
		}
	}()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	h.runPublicLive(ctx, r.Context(), stream, limit, offset, func(_ context.Context, body []byte) error {
		return writePublicFrame(w, body)
	})
}

func (h *handler) publicWebSocket(w http.ResponseWriter, r *http.Request, requestID string) {
	// Compare the raw browser Origin with the configured canonical origin. Do
	// not trust Host, normalize attacker input or accept an absent Origin.
	if h.origin == "" || len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != h.origin {
		fail(w, http.StatusForbidden, "forbidden", "A trusted Origin is required", requestID)
		return
	}
	stream, limit, offset, ok := h.admitPublicStream(w, r, requestID)
	if !ok {
		return
	}
	defer h.releasePublicStream(stream)
	controller := http.NewResponseController(w)
	stream.setWriter(func() error { return controller.SetWriteDeadline(time.Now().Add(10 * time.Second)) }, func() { _ = controller.SetWriteDeadline(time.Now()) })
	upgradeWriter := &liveUpgradeWriter{ResponseWriter: w, stream: stream}
	var connection *websocket.Conn
	err := stream.write(stream.ctx, false, func() error {
		var acceptErr error
		connection, acceptErr = websocket.Accept(upgradeWriter, r, &websocket.AcceptOptions{
			OriginPatterns:  []string{h.origin},
			CompressionMode: websocket.CompressionDisabled,
		})
		return acceptErr
	})
	if err != nil {
		return
	}
	var stopRead context.CancelFunc
	var readDone chan struct{}
	defer func() {
		if stopRead != nil {
			stopRead()
		}
		_ = connection.CloseNow()
		if readDone != nil {
			<-readDone
		}
	}()
	connection.SetReadLimit(1024)
	stream.setWriter(nil, func() { _ = connection.CloseNow() })
	// A bounded HTTP handshake must not leave an idle WS with a ten-second
	// socket deadline. Subsequent library writes use individual contexts.
	if err := upgradeWriter.connection.SetDeadline(time.Time{}); err != nil {
		return
	}
	ctx, cancel := h.publicStreamContext(r, stream)
	defer cancel()
	// v1.8.15 CloseRead waits for its own goroutine on unexpected data.
	// Use the library reader once: any application message ends this readonly
	// channel without draining an untrusted payload or waiting for a handshake.
	readCtx, stopRead := context.WithCancel(context.Background())
	readDone = make(chan struct{})
	go func() {
		defer close(readDone)
		_, _, _ = connection.Reader(readCtx)
		stopRead()
		cancel()
	}()
	h.runPublicLive(ctx, readCtx, stream, limit, offset, func(writeCtx context.Context, body []byte) error {
		return connection.Write(writeCtx, websocket.MessageText, body)
	})
}

func writePublicFrame(w http.ResponseWriter, body []byte) error {
	controller := http.NewResponseController(w)
	// Write framing separately instead of allocating and copying a second
	// full page just to prepend the SSE event name.
	if n, err := io.WriteString(w, "event: message\ndata: "); err != nil || n != len("event: message\ndata: ") {
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	n, err := w.Write(body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	if n, err := io.WriteString(w, "\n\n"); err != nil || n != 2 {
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	if err := controller.Flush(); err != nil {
		return err
	}
	// HTTP/2 also applies deadlines while idle. Each frame gets its own
	// deadline; heartbeats must not inherit the preceding frame's deadline.
	return controller.SetWriteDeadline(time.Time{})
}
