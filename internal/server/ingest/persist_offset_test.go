package ingest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func TestPersistOffsetDeterminismAndRange(t *testing.T) {
	for _, interval := range []time.Duration{time.Nanosecond, 7 * time.Nanosecond, time.Second, 30 * time.Second, time.Duration(1<<63 - 1)} {
		for i := range 200 {
			id := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
			got := persistOffset(id, interval)
			if got < 0 || got >= interval || got != persistOffset(id, interval) {
				t.Fatalf("id=%s interval=%s offset=%s", id, interval, got)
			}
		}
	}
	// 固定向量同时锁定哈希算法和字节序，跨服务重启保持相同偏移。
	if got := persistOffset("00000000-0000-4000-8000-000000000000", 30*time.Second); got != 25647491418*time.Nanosecond {
		t.Fatalf("offset=%s", got)
	}
}

func TestPersistOffsetDistribution(t *testing.T) {
	const interval = 30 * time.Second
	var buckets [30]int
	offsets := make(map[time.Duration]bool)
	for i := range 200 {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
		offset := persistOffset(id, interval)
		offsets[offset] = true
		buckets[offset/time.Second]++
	}
	occupied := 0
	for _, count := range buckets {
		if count > 0 {
			occupied++
		}
		if count > 20 {
			t.Fatalf("offsets concentrated: %v", buckets)
		}
	}
	if occupied < 27 || len(offsets) < 190 {
		t.Fatalf("offsets not spread: unique=%d buckets=%v", len(offsets), buckets)
	}
}

func TestPersistenceDueWithOffset(t *testing.T) {
	id := "00000000-0000-4000-8000-000000000000"
	base := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{})
	offset := persistOffset(id, 30*time.Second)
	if s.persistInterval != 30*time.Second || s.disablePersistOffset {
		t.Fatal("production defaults do not enable offset")
	}
	for _, test := range []struct {
		elapsed time.Duration
		due     bool
	}{
		{-time.Second, false}, {30 * time.Second, false},
		{30*time.Second + offset - time.Nanosecond, false},
		{30*time.Second + offset, true}, {60 * time.Second, true},
	} {
		if got := s.persistenceDue(id, base, base.Add(test.elapsed)); got != test.due {
			t.Fatalf("elapsed=%s due=%v want %v", test.elapsed, got, test.due)
		}
	}
	if !s.persistenceDue(id, time.Time{}, base) {
		t.Fatal("first persistence delayed")
	}
	disabled := NewService(nil, Options{DisablePersistOffset: true})
	if disabled.persistenceDue(id, base, base.Add(30*time.Second-time.Nanosecond)) || !disabled.persistenceDue(id, base, base.Add(30*time.Second)) {
		t.Fatal("disabled offset changed base interval")
	}
	// 大间隔不能因基础间隔与偏移相加溢出而提前到期。
	large := NewService(nil, Options{PersistInterval: time.Duration(1<<63 - 1)})
	if large.persistenceDue(id, base, base.Add(time.Duration(1<<63-1))) {
		t.Fatal("large interval overflowed")
	}
}

func TestPersistOffsetFastPathAndDue(t *testing.T) {
	f := newCoalesceFixtureWithOptions(t, Options{})
	ctx := context.Background()
	first := f.now
	interval := f.service.persistInterval + persistOffset(f.id, f.service.persistInterval)
	if f.writes(t) != 1 {
		t.Fatal("first report was delayed")
	}
	// 占满写入准入，证明偏移内的报告仍然使用不依赖写入队列的快路径。
	for range cap(f.service.writerQueue) {
		f.service.writerQueue <- struct{}{}
	}
	f.now = first.Add(interval - time.Nanosecond)
	f.report.Seq++
	_, err := acceptReport(ctx, f.service, coalesceToken, f.report)
	for range cap(f.service.writerQueue) {
		<-f.service.writerQueue
	}
	if err != nil || f.writes(t) != 1 {
		t.Fatalf("fast path: writes=%d err=%v", f.writes(t), err)
	}
	latest, err := f.service.Latest(ctx, f.id)
	if err != nil || latest.Seq != 2 || !latest.ReceivedAt.Equal(f.now) {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	f.now = first.Add(interval)
	f.report.Seq++
	if _, err := acceptReport(ctx, f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	var seen string
	if err := f.db.QueryRow("SELECT last_seen FROM nodes WHERE id=?", f.id).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if f.writes(t) != 2 || seen != timekey.Format(f.now) {
		t.Fatalf("due writes=%d seen=%s", f.writes(t), seen)
	}
	// 下一个周期使用相同偏移，而不是重新抽样。
	f.now = f.now.Add(interval - time.Nanosecond)
	f.report.Seq++
	if _, err := acceptReport(ctx, f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	if f.writes(t) != 2 {
		t.Fatal("second cycle persisted before fixed offset")
	}
	f.now = f.now.Add(time.Nanosecond)
	f.report.Seq++
	if _, err := acceptReport(ctx, f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	if f.writes(t) != 3 {
		t.Fatal("second cycle did not persist at fixed offset")
	}
}

func TestPersistOffsetImmediateChanges(t *testing.T) {
	for _, mutation := range []string{"run", "credential", "runtime", "system", "restart"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCoalesceFixtureWithOptions(t, Options{})
			f.now = f.now.Add(time.Second)
			f.report.Seq++
			token := coalesceToken
			switch mutation {
			case "run":
				f.report.RunGeneration++
				f.report.SessionID = "next"
				f.report.Seq = 1
			case "credential":
				token = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
				// 单节点数据库内替换唯一 token_hash，并推进凭据代次。
				// 不创建第二个共用凭据的节点，避免唯一约束掩盖被测行为。
				credential := sha256.Sum256([]byte(token))
				if _, err := f.db.Exec("UPDATE nodes SET token_hash=?,credential_epoch=credential_epoch+1 WHERE id=?", credential[:], f.id); err != nil {
					t.Fatal(err)
				}
			case "runtime":
				f.report.Runtime.ReportIntervalSeconds = 5
			case "system":
				f.report.System.Hostname = "changed"
			case "restart":
				f.service = NewService(f.nodes.Database(), Options{Now: func() time.Time { return f.now }})
			}
			if _, err := acceptReport(context.Background(), f.service, token, f.report); err != nil {
				t.Fatal(err)
			}
			if f.writes(t) != 2 {
				t.Fatal("change delayed by offset")
			}
		})
	}
}

func TestPersistOffsetPublicLivenessAndRestart(t *testing.T) {
	f := newCoalesceFixtureWithOptions(t, Options{})
	ctx := context.Background()
	persisted := f.now
	f.now = f.now.Add(f.service.persistInterval + persistOffset(f.id, f.service.persistInterval) - time.Nanosecond)
	f.report.Seq++
	if _, err := acceptReport(ctx, f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	received := f.now
	for _, test := range []struct {
		elapsed time.Duration
		status  string
	}{
		{0, "online"}, {nodes.OnlineWindow, "online"}, {nodes.OnlineWindow + time.Nanosecond, "offline"},
	} {
		public, err := publicNode(ctx, f.nodes, f.service, f.id, received.Add(test.elapsed))
		if err != nil || public.Status != test.status || public.LastSeen == nil || !public.LastSeen.Equal(received) {
			t.Fatalf("public=%+v err=%v", public, err)
		}
	}
	if f.writes(t) != 1 {
		t.Fatal("liveness reads persisted a report")
	}
	restarted := NewService(f.nodes.Database(), Options{Now: func() time.Time { return f.now }})
	public, err := publicNode(ctx, f.nodes, restarted, f.id, f.now)
	if err != nil || public.Status != "online" || public.CPUPercent != nil || public.LastSeen == nil || !public.LastSeen.Equal(persisted) {
		t.Fatalf("restart public=%+v err=%v", public, err)
	}
	if _, err := restarted.Latest(ctx, f.id); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("restart latest err=%v", err)
	}
	f.service = restarted
	f.report.Seq++
	if _, err := acceptReport(ctx, f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	if f.writes(t) != 2 {
		t.Fatal("first report after restart delayed")
	}
}

func TestPersistOffsetBuffersTrafficAndProbesUntilDue(t *testing.T) {
	target := "tcp://offset.example.test:443"
	f := newCoalesceFixtureWithOptions(t, Options{}, target)
	ctx := context.Background()
	first := f.now
	interval := f.service.persistInterval + persistOffset(f.id, f.service.persistInterval)
	// 默认最长周期不足 60 秒；每秒一个探测不会超过每目标 64 条上限。
	count := int((interval - time.Nanosecond) / time.Second)
	for i := 1; i <= count; i++ {
		f.now = first.Add(time.Duration(i) * time.Second)
		f.report.Seq++
		f.report.Traffic = []v1.Traffic{trafficEntry("a", first.Truncate(time.Hour), fmt.Sprint(i), fmt.Sprint(i*2))}
		f.report.ProbeResults = []v1.ProbeResult{probeResult(target, f.now, true)}
		response, err := acceptReport(ctx, f.service, coalesceToken, f.report)
		if err != nil || len(response.TrafficAcks) != 0 {
			t.Fatalf("uncommitted ack=%+v err=%v", response, err)
		}
	}
	if f.writes(t) != 1 {
		t.Fatal("buffered report persisted before due")
	}
	state := f.service.acquireExistingState(f.id)
	state.mu.Lock()
	traffic, probes := len(state.traffic.pending), len(state.probes.targets[target].pending)
	state.mu.Unlock()
	f.service.releaseState(state)
	if traffic != 1 || probes != count || probes >= probePendingLimit {
		t.Fatalf("traffic=%d probes=%d count=%d", traffic, probes, count)
	}
	for _, table := range []string{"traffic_hourly", "probe_latest", "probe_hourly"} {
		var rows int
		if err := f.db.QueryRow("SELECT count(*) FROM "+table+" WHERE node_id=?", f.id).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("%s rows=%d err=%v", table, rows, err)
		}
	}
	f.now = first.Add(interval)
	f.report.Seq++
	f.report.ProbeResults = nil
	response, err := acceptReport(ctx, f.service, coalesceToken, f.report)
	if err != nil || len(response.TrafficAcks) != 1 || response.TrafficAcks[0].RxBytes != v1.DecimalUint64(fmt.Sprint(count)) {
		t.Fatalf("committed ack=%+v err=%v", response, err)
	}
	checks, failures, latest := probeTotals(t, f, target)
	if checks != count || failures != 0 || latest != timekey.Format(first.Add(time.Duration(count)*time.Second)) {
		t.Fatalf("checks=%d failures=%d latest=%s", checks, failures, latest)
	}
	var rx, tx int
	if err := f.db.QueryRow("SELECT rx_bytes,tx_bytes FROM traffic_hourly WHERE node_id=?", f.id).Scan(&rx, &tx); err != nil || rx != count || tx != count*2 {
		t.Fatalf("traffic=%d/%d err=%v", rx, tx, err)
	}
}
