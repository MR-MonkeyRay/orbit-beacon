package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/uuid"
)

type consoleFleetData struct {
	Nodes []nodes.LiveNode `json:"nodes"`
}

type consoleDetailData struct {
	Node     *nodes.Node       `json:"node"`
	Latest   *latestSampleView `json:"latest"`
	identity nodes.LiveIdentity
	Runtime  *v1.RuntimeSettings `json:"runtime"`
}

func consoleNodeID(r *http.Request) (string, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", nodes.ErrInvalidInput
	}
	for key, values := range query {
		if key != "node_id" || len(values) != 1 || !uuid.ValidUUID(values[0]) {
			return "", nodes.ErrInvalidInput
		}
	}
	return query.Get("node_id"), nil
}

func (h *handler) consoleFleet(ctx context.Context) (consoleFleetData, error) {
	records, err := h.nodes.LiveRecords(ctx)
	if err != nil {
		return consoleFleetData{}, err
	}
	data := consoleFleetData{Nodes: make([]nodes.LiveNode, len(records))}
	now := h.now()
	for i := range records {
		data.Nodes[i] = h.ingest.ProjectLiveRecord(&records[i], now)
	}
	return data, nil
}

func (h *handler) consoleDetail(ctx context.Context, id string) (consoleDetailData, error) {
	record, err := h.nodes.LiveDetail(ctx, id)
	if errors.Is(err, nodes.ErrNotFound) {
		return consoleDetailData{}, nil
	}
	if err != nil {
		return consoleDetailData{}, err
	}
	latest, err := h.ingest.ProjectLiveDetail(&record, h.now())
	if err != nil {
		return consoleDetailData{}, err
	}
	data := consoleDetailData{Node: &record.Node, Runtime: record.Runtime, identity: record.Identity}
	if latest != nil {
		data.Latest = &latestSampleView{ReceivedAt: latest.ReceivedAt, SampledAt: latest.SampledAt, Seq: latest.Seq, Metrics: latest.Metrics, System: latest.System, Runtime: latest.Runtime}
	}
	return data, nil
}

func consoleFleetsEqual(a, b consoleFleetData) bool {
	return slices.EqualFunc(a.Nodes, b.Nodes, func(a, b nodes.LiveNode) bool {
		return a.ID == b.ID && a.Status == b.Status && a.Presence == b.Presence && a.AgentVersion == b.AgentVersion && equalLiveTime(a.LastSeen, b.LastSeen)
	})
}

func consoleDetailsEqual(a, b consoleDetailData) bool {
	if a.identity != b.identity {
		return false
	}
	if (a.Node == nil) != (b.Node == nil) {
		return false
	}
	if a.Node != nil {
		a, b := a.Node, b.Node
		if a.ID != b.ID || a.Name != b.Name || a.Status != b.Status || a.Presence != b.Presence || a.Notes != b.Notes || a.Region != b.Region || a.Provider != b.Provider || a.AgentVersion != b.AgentVersion || !equalLiveValue(a.GroupID, b.GroupID) || !slices.Equal(a.Tags, b.Tags) || !a.CreatedAt.Equal(b.CreatedAt) || !equalLiveTime(a.LastSeen, b.LastSeen) {
			return false
		}
	}
	if (a.Latest == nil) != (b.Latest == nil) || (a.Runtime == nil) != (b.Runtime == nil) {
		return false
	}
	// Accepted sequence and receipt identify an immutable sample; there is no
	// reason to walk all metric maps just to suppress an unchanged snapshot.
	if a.Latest != nil && (a.Latest.Seq != b.Latest.Seq || !a.Latest.ReceivedAt.Equal(b.Latest.ReceivedAt) || !a.Latest.SampledAt.Equal(b.Latest.SampledAt)) {
		return false
	}
	return a.Runtime == nil || a.Runtime.ReportIntervalSeconds == b.Runtime.ReportIntervalSeconds && a.Runtime.DiskIntervalSeconds == b.Runtime.DiskIntervalSeconds && slices.Equal(a.Runtime.Interfaces, b.Runtime.Interfaces) && slices.Equal(a.Runtime.DiskPaths, b.Runtime.DiskPaths) && slices.Equal(a.Runtime.ProbeTargets, b.Runtime.ProbeTargets)
}

// Identity changes share the public safety version: each projection is checked
// both here and at egress. Ordinary metadata changes remain cadence-bound.
func consoleSnapshot[T any](h *handler, ctx context.Context, project func(context.Context) (T, error)) (T, uint64, error) {
	for range 3 {
		version := h.nodes.PublicVisibilityVersion()
		data, err := project(ctx)
		if err != nil {
			return data, version, err
		}
		if h.nodes.PublicVisibilityVersion() == version {
			return data, version, nil
		}
	}
	var zero T
	return zero, 0, errSnapshotBusy
}

func (h *handler) consoleLive(w http.ResponseWriter, r *http.Request, requestID, token string, session auth.Session) {
	id, err := consoleNodeID(r)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	if r.URL.Path == "/api/v1/live/ws" && !h.trustedLiveOrigin(w, r, requestID) {
		return
	}
	if id == "" {
		serveConsoleLive(h, w, r, requestID, token, session.ExpiresAt, h.consoleFleet, consoleFleetsEqual)
	} else {
		serveConsoleLive(h, w, r, requestID, token, session.ExpiresAt, func(ctx context.Context) (consoleDetailData, error) { return h.consoleDetail(ctx, id) }, consoleDetailsEqual)
	}
}

func serveConsoleLive[T any](h *handler, w http.ResponseWriter, r *http.Request, requestID, token string, expires time.Time, project func(context.Context) (T, error), equal func(T, T) bool) {
	authenticate := func(ctx context.Context) error { _, err := h.auth.Authenticate(ctx, token); return err }
	source := liveSource[T]{snapshot: func(ctx context.Context) (T, uint64, error) { return consoleSnapshot(h, ctx, project) }, equal: equal, authenticate: authenticate}
	if r.URL.Path == "/api/v1/live" {
		if err := authenticate(r.Context()); err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		data, version, err := source.snapshot(r.Context())
		if err != nil {
			if errors.Is(err, errSnapshotBusy) {
				w.Header().Set("Retry-After", strconv.Itoa(h.liveSeconds()))
				fail(w, http.StatusServiceUnavailable, "snapshot_busy", "Node inventory is changing; retry later", requestID)
			} else {
				h.serviceError(w, err, requestID)
			}
			return
		}
		body, err := json.Marshal(snapshotFrame(h, data))
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		// Recheck after projection and encoding, immediately before private bytes.
		if err := authenticate(r.Context()); err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		if h.nodes.PublicVisibilityVersion() != version {
			w.Header().Set("Retry-After", strconv.Itoa(h.liveSeconds()))
			fail(w, http.StatusServiceUnavailable, "snapshot_busy", "Node inventory is changing; retry later", requestID)
			return
		}
		writeJSONBody(w, http.StatusOK, body)
		return
	}
	stream, ok := h.streams.acquire(h.publicStreamIP(r))
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(h.liveSeconds()))
		fail(w, http.StatusServiceUnavailable, "stream_busy", "Live stream capacity is unavailable", requestID)
		return
	}
	defer stream.release()
	run := func(ctx, request context.Context, send func(context.Context, []byte) error) {
		runLive(h, ctx, request, stream, source, send)
	}
	if r.URL.Path == "/api/v1/live/ws" {
		h.serveLiveWebSocket(w, r, stream, expires, run)
	} else {
		h.serveLiveSSE(w, r, stream, expires, run)
	}
}
