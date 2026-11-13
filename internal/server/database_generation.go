package server

import (
	"context"

	"orbitbeacon/internal/server/storage"
)

// PrepareDatabaseSwitch 暂停而非永久关闭流；新世代发布前不提供旧公开投影。
func (h *handler) PrepareDatabaseSwitch(ctx context.Context) error {
	h.publicPageMu.Lock()
	h.publicPageUnknown.Store(true)
	h.nodes.InvalidatePublicSafety()
	h.streams.mu.Lock()
	h.streams.paused = true
	streams := make([]*liveStream, 0, len(h.streams.active))
	for stream := range h.streams.active {
		streams = append(streams, stream)
	}
	h.streams.mu.Unlock()
	h.publicPageMu.Unlock()
	for _, stream := range streams {
		stream.stop(false)
	}
	return h.WaitLiveStreams(ctx)
}

// PublishDatabaseGeneration 使用协调器提供的候选租约，不重新等待公共准入。
func (h *handler) PublishDatabaseGeneration(ctx context.Context, lease *storage.Lease) error {
	h.publicPageMu.Lock()
	defer h.publicPageMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	readCtx, cancel := context.WithCancel(lease.Context())
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	settings, err := h.management.PublicPageSettings(readCtx)
	if err != nil {
		return err
	}
	if err = readCtx.Err(); err != nil {
		return err
	}
	h.publicPageDisabled.Store(!settings.Enabled)
	h.publicPageShowGroups.Store(settings.ShowGroups)
	h.nodes.InvalidatePublicSafety()
	h.publicPageUnknown.Store(false)
	h.streams.mu.Lock()
	h.streams.paused = false
	h.streams.mu.Unlock()
	return nil
}
