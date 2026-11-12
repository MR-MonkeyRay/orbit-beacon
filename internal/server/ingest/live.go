package ingest

import (
	"encoding/json"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

// LiveReceivedAt 复用实时投影的身份和存储世代围栏，不复制或解码报告。
func (s *Service) LiveReceivedAt(id string, identity *nodes.LiveIdentity) (time.Time, bool) {
	_, received, ok := s.liveSampleSnapshot(id, identity)
	return received, ok
}

// ProjectLiveRecord 在身份围栏内读取服务端接收时间和写入时计算的指标摘要。
// 计算节点列表的在线状态无需分配报告副本或解码报告。
func (s *Service) ProjectLiveRecord(record *nodes.LiveRecord, now time.Time) nodes.LiveNode {
	out := record.LiveNode
	out.CPUPercent, out.MemoryUsedPercent = nil, nil
	identity := &record.Identity
	if identity.HasCredential && identity.Generation != 0 && identity.Session != "" {
		state := s.acquireExistingState(record.ID)
		if state != nil {
			state.mu.Lock()
			if s.db.Ready() && state.storageGeneration != 0 && state.storageGeneration == s.db.Generation() && state.matchesIdentity(identity.Credential[:], identity.CredentialEpoch, identity.Generation, identity.Session) {
				if out.LastSeen == nil || state.receivedAt.After(*out.LastSeen) {
					seen := state.receivedAt
					out.LastSeen = &seen
				}
				if nodes.Presence(out.Status, identity.HasCredential, out.LastSeen, now) == "online" {
					out.CPUPercent, out.MemoryUsedPercent = state.cpuPercent, state.memoryUsedPercent
				}
			}
			state.mu.Unlock()
			s.releaseState(state)
		}
	}
	out.Presence = nodes.Presence(out.Status, identity.HasCredential, out.LastSeen, now)
	return out
}

// liveMetricValue 复用公开摘要的校验和内存口径，但舰队仅显示报告状态为 ok 的值。
// 返回值发布后不可变，与 latest 在同一把锁下替换。
func liveMetricValue(metric publicMetric) *float64 {
	if !metric.valid || metric.status != "ok" {
		return nil
	}
	return new(metric.value)
}

// ProjectLiveDetail 使用同一持久化行中的身份围栏投影最近接受的样本。
func (s *Service) ProjectLiveDetail(record *nodes.LiveDetailRecord, now time.Time) (*LatestReport, error) {
	identity := &record.Identity
	if !identity.HasCredential || identity.Generation == 0 || identity.Session == "" {
		record.Node.Presence = nodes.Presence(record.Node.Status, identity.HasCredential, record.Node.LastSeen, now)
		return nil, nil
	}
	body, received, ok := s.liveSampleSnapshot(record.Node.ID, identity)
	if !ok {
		return nil, nil
	}
	if record.Node.LastSeen == nil || received.After(*record.Node.LastSeen) {
		record.Node.LastSeen = &received
	}
	record.Node.Presence = nodes.Presence(record.Node.Status, identity.HasCredential, record.Node.LastSeen, now)
	var report v1.Report
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, err
	}
	return &LatestReport{Report: report, ReceivedAt: received}, nil
}

// ProjectLiveSample 使用清单已读取的身份围栏，不执行 SQL。
func (s *Service) ProjectLiveSample(record *nodes.LiveRecord) (*LatestReport, error) {
	body, received, ok := s.liveSampleSnapshot(record.ID, &record.Identity)
	if !ok {
		return nil, nil
	}
	var report v1.Report
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, err
	}
	return &LatestReport{Report: report, ReceivedAt: received}, nil
}

// liveSampleSnapshot 与详情投影共用完整身份检查。已发布报告缓冲区不可变，
// 因此释放状态锁后可以安全解码，且不会读取替换身份的旧样本。
func (s *Service) liveSampleSnapshot(id string, identity *nodes.LiveIdentity) ([]byte, time.Time, bool) {
	if !identity.HasCredential || identity.Generation == 0 || identity.Session == "" {
		return nil, time.Time{}, false
	}
	state := s.acquireExistingState(id)
	if state == nil {
		return nil, time.Time{}, false
	}
	defer s.releaseState(state)
	state.mu.Lock()
	defer state.mu.Unlock()
	if !s.db.Ready() || state.storageGeneration == 0 || state.storageGeneration != s.db.Generation() || !state.matchesIdentity(identity.Credential[:], identity.CredentialEpoch, identity.Generation, identity.Session) {
		return nil, time.Time{}, false
	}
	return state.latest, state.receivedAt, true
}
