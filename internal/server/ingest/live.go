package ingest

import (
	"encoding/json"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

// ProjectLiveRecord 在现有状态锁下仅读取服务端接收时间。
// 计算节点列表的在线状态无需分配报告副本或解码报告。
func (s *Service) ProjectLiveRecord(record *nodes.LiveRecord, now time.Time) nodes.LiveNode {
	out := record.LiveNode
	identity := &record.Identity
	if identity.HasCredential && identity.Generation != 0 && identity.Session != "" {
		state := s.acquireExistingState(record.ID)
		if state != nil {
			state.mu.Lock()
			if state.matchesIdentity(identity.Credential[:], identity.CredentialEpoch, identity.Generation, identity.Session) && (out.LastSeen == nil || state.receivedAt.After(*out.LastSeen)) {
				seen := state.receivedAt
				out.LastSeen = &seen
			}
			state.mu.Unlock()
			s.releaseState(state)
		}
	}
	out.Presence = nodes.Presence(out.Status, identity.HasCredential, out.LastSeen, now)
	return out
}

// ProjectLiveDetail 使用同一持久化行中的身份和观测数据。即使新事务已提交但内存尚未发布，
// runtime 也必须与返回的已接受样本一致。重启后无内存样本时可单独使用持久化 runtime。
func (s *Service) ProjectLiveDetail(record *nodes.LiveDetailRecord, now time.Time) (*LatestReport, error) {
	identity := &record.Identity
	if !identity.HasCredential || identity.Generation == 0 || identity.Session == "" {
		record.Node.Presence = nodes.Presence(record.Node.Status, identity.HasCredential, record.Node.LastSeen, now)
		return nil, nil
	}
	state := s.acquireExistingState(record.Node.ID)
	if state == nil {
		return nil, nil
	}
	defer s.releaseState(state)
	state.mu.Lock()
	if !state.matchesIdentity(identity.Credential[:], identity.CredentialEpoch, identity.Generation, identity.Session) {
		state.mu.Unlock()
		return nil, nil
	}
	// 已发布的报告缓冲区不可变，因此解码这一条 detail 时持有引用即可，
	// 即使有新报告到达。
	body := state.latest
	received := state.receivedAt
	state.mu.Unlock()
	if record.Node.LastSeen == nil || received.After(*record.Node.LastSeen) {
		record.Node.LastSeen = &received
	}
	record.Node.Presence = nodes.Presence(record.Node.Status, identity.HasCredential, record.Node.LastSeen, now)
	var report v1.Report
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, err
	}
	record.Runtime = &report.Runtime
	return &LatestReport{Report: report, ReceivedAt: received}, nil
}
