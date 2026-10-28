package server

import (
	"context"
	"net/http"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

// latestSampleView excludes credentials and process identity from private reads.
// Its timestamps and original metric metadata describe the accepted sample, not
// the response clock or the node's current presence.
type latestSampleView struct {
	ReceivedAt time.Time          `json:"received_at"`
	SampledAt  time.Time          `json:"sampled_at"`
	Seq        uint64             `json:"seq"`
	Metrics    v1.Metrics         `json:"metrics"`
	System     *v1.System         `json:"system"`
	Runtime    v1.RuntimeSettings `json:"runtime"`
}

func (h *handler) latestSample(ctx context.Context, id string) (latestSampleView, error) {
	latest, err := h.ingest.Latest(ctx, id)
	if err != nil {
		return latestSampleView{}, err
	}
	return latestSampleView{
		ReceivedAt: latest.ReceivedAt,
		SampledAt:  latest.SampledAt,
		Seq:        latest.Seq,
		Metrics:    latest.Metrics,
		System:     latest.System,
		Runtime:    latest.Runtime,
	}, nil
}

func (h *handler) nodeLatest(w http.ResponseWriter, r *http.Request, id, requestID string) {
	latest, err := h.latestSample(r.Context(), id)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		latestSampleView
		ServerTime time.Time `json:"server_time"`
	}{latestSampleView: latest, ServerTime: h.now().UTC()})
}
