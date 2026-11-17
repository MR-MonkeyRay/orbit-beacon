// Package change 提供基于关闭 channel 广播的轻量级状态变更通知器。
package change

import "sync"

// Notifier 通过关闭当前 channel 来广播变更。
type Notifier struct {
	mu      sync.Mutex
	version uint64
	changed chan struct{}
}

// Snapshot 返回当前单调递增的版本号及下一次变更时会被关闭的 channel。
func (n *Notifier) Snapshot() (uint64, <-chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.changed == nil {
		n.changed = make(chan struct{})
	}
	return n.version, n.changed
}

// Notify 递增版本号并关闭当前监听 channel，唤醒所有等待中的订阅方。
func (n *Notifier) Notify() {
	n.mu.Lock()
	n.version++
	if n.changed != nil {
		close(n.changed)
	}
	n.changed = make(chan struct{})
	n.mu.Unlock()
}
