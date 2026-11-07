package collect

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

// 每个桶保留创建时的 epoch，因此轮换 epoch 不会让已有桶的累计总量
// 以新身份再次上报。
type trafficBucket struct {
	epoch  string
	rx, tx uint64
	closed bool
	acked  bool
}

// TrafficAccumulator 按小时保存本进程的累计流量，保留的小时数有上限。
// 采集与上报可安全并发访问；now 可注入以便测试。
type TrafficAccumulator struct {
	mu           sync.Mutex
	now          func() time.Time
	epoch        string
	previous     map[string]NetworkCounter
	buckets      map[time.Time]*trafficBucket
	enabled      bool
	newestPeriod time.Time
}

func NewTrafficAccumulator(now func() time.Time) *TrafficAccumulator {
	if now == nil {
		now = time.Now
	}
	return &TrafficAccumulator{now: now, epoch: newTrafficEpoch(), previous: make(map[string]NetworkCounter), buckets: make(map[time.Time]*trafficBucket)}
}

func newTrafficEpoch() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

// Observe 只使用实际读取的计数器值，不使用缓存的速率观测。
// 已选网卡在读失败时保留基线；取消选择则丢弃基线，
// 避免之后重新选中时把未选中区间的流量计入。
func (a *TrafficAccumulator) Observe(selected []string, counters map[string]NetworkCounter) {
	a.ObserveAt(a.now(), selected, counters)
}

// ObserveAt 使用计数器读数的时间戳，即使处理跨越了小时边界。
// 没有时间戳时，Observe 提供可注入的时钟。
// 早于最新已观测小时的样本计入最新小时。
func (a *TrafficAccumulator) ObserveAt(at time.Time, selected []string, counters map[string]NetworkCounter) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for name := range a.previous {
		if !slices.Contains(selected, name) {
			delete(a.previous, name)
		}
	}
	if len(counters) == 0 {
		return
	}
	a.enabled = true
	period := a.advance(at)
	bucket := a.bucket(period)
	for _, name := range selected {
		current, ok := counters[name]
		if !ok {
			continue
		}
		if previous, ok := a.previous[name]; ok {
			rx, tx := counterDelta(current.RxBytes, previous.RxBytes), counterDelta(current.TxBytes, previous.TxBytes)
			if rx != 0 || tx != 0 {
				bucket.rx, bucket.tx = trafficSum(bucket.rx, rx), trafficSum(bucket.tx, tx)
				bucket.acked = false
			}
		}
		a.previous[name] = current
	}
	for start, bucket := range a.buckets {
		if start.Before(period) {
			bucket.closed = true
			if bucket.acked {
				delete(a.buckets, start)
			}
		}
	}
	a.trim()
}

func counterDelta(current, previous uint64) uint64 {
	if current < previous {
		return current
	}
	return current - previous
}

// 上报总量不能超过 SQLite INTEGER 的上限。达到上限后不再增加，
// 防止异常的内核计数器值溢出、导致累计总量减少或阻塞所有上报。
func trafficSum(total, delta uint64) uint64 {
	return total + min(delta, uint64(math.MaxInt64)-total)
}

// period 在系统时钟小幅回退时仍沿用最新采样小时。上报与确认使用同一边界，
// 防止重建已结算的小时。调用方持有 mu。
// 大幅回退则视为之前发生了异常的时钟前跳；若仍沿用原值，
// 计数会滞留在服务器不接受的未来小时中。
func (a *TrafficAccumulator) period(at time.Time) time.Time {
	period := at.UTC().Truncate(time.Hour)
	if period.Before(a.newestPeriod) && a.newestPeriod.Sub(period) <= time.Hour {
		return a.newestPeriod
	}
	return period
}

// advance 根据采样时间更新最新小时。调用方持有 mu。
func (a *TrafficAccumulator) advance(at time.Time) time.Time {
	period := a.recover(at)
	a.newestPeriod = period
	return period
}

// recover 处理异常时钟前跳后的恢复。采样与上报都会调用它，
// 避免在旧 epoch 下重建已结算的小时。上报不会推进最新采样小时，
// 因而延迟处理的样本仍计入采集时所属的小时。调用方持有 mu。
func (a *TrafficAccumulator) recover(at time.Time) time.Time {
	period := a.period(at)
	if period.Before(a.newestPeriod) {
		// 服务器不会接受未来小时，这些桶只会占位直到被裁剪。
		for start := range a.buckets {
			if start.After(period) {
				delete(a.buckets, start)
			}
		}
		// 前跳期间结算的小时可能被从零重建；服务器按 epoch 取 MAX，
		// 沿用旧 epoch 会隐藏新增字节。
		a.epoch = newTrafficEpoch()
		a.newestPeriod = period
	}
	return period
}

func (a *TrafficAccumulator) bucket(period time.Time) *trafficBucket {
	bucket := a.buckets[period]
	if bucket == nil {
		bucket = &trafficBucket{epoch: a.epoch}
		a.buckets[period] = bucket
	}
	return bucket
}

func (a *TrafficAccumulator) periods() []time.Time {
	periods := make([]time.Time, 0, len(a.buckets))
	for period := range a.buckets {
		periods = append(periods, period)
	}
	slices.SortFunc(periods, func(a, b time.Time) int { return a.Compare(b) })
	return periods
}

func (a *TrafficAccumulator) trim() {
	periods := a.periods()
	for _, period := range periods[:max(0, len(periods)-v1.MaxTrafficEntries)] {
		delete(a.buckets, period)
	}
}

// Entries 返回独立快照，按时间从早到晚排列。未采样的当前小时
// （系统时钟小幅回退时沿用最新采样小时）仅在有空位时以零值上报，
// 不会挤掉已采样的桶。
// 不支持的平台不启用累计，继续发送 []。
func (a *TrafficAccumulator) Entries() []v1.Traffic {
	a.mu.Lock()
	defer a.mu.Unlock()
	entries := make([]v1.Traffic, 0, len(a.buckets))
	if !a.enabled {
		return entries
	}
	current := a.recover(a.now())
	for _, period := range a.periods() {
		bucket := a.buckets[period]
		entries = append(entries, v1.Traffic{Epoch: bucket.epoch, PeriodStart: period, RxBytes: v1.DecimalUint64(strconv.FormatUint(bucket.rx, 10)), TxBytes: v1.DecimalUint64(strconv.FormatUint(bucket.tx, 10))})
	}
	// 所有桶都不晚于 newestPeriod，而 recover 保证它不晚于当前时间，
	// 因此零值条目始终位于最后。
	if a.buckets[current] == nil && len(entries) < v1.MaxTrafficEntries {
		entries = append(entries, v1.Traffic{Epoch: a.epoch, PeriodStart: current, RxBytes: "0", TxBytes: "0"})
	}
	return entries
}

// Acknowledge 记录服务器确认的准确总量，包括当前小时的确认。
// 仅跨过小时边界不足以结算桶：还须后续观测将其关闭，
// 确保最后的增量已计入样本所属的小时。
func (a *TrafficAccumulator) Acknowledge(acks []v1.Traffic) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.period(a.now())
	for _, ack := range acks {
		bucket := a.buckets[ack.PeriodStart]
		if bucket == nil || ack.Epoch != bucket.epoch || string(ack.RxBytes) != strconv.FormatUint(bucket.rx, 10) || string(ack.TxBytes) != strconv.FormatUint(bucket.tx, 10) {
			continue
		}
		bucket.acked = true
		if bucket.closed && ack.PeriodStart.Before(current) {
			delete(a.buckets, ack.PeriodStart)
		}
	}
}
