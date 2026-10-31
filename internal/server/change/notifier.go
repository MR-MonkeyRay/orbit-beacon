package change

import "sync"

// Notifier 通过关闭当前 channel 来广播变更。
type Notifier struct {
	mu      sync.Mutex
	version uint64
	changed chan struct{}
}

func (n *Notifier) Snapshot() (uint64, <-chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.changed == nil {
		n.changed = make(chan struct{})
	}
	return n.version, n.changed
}

func (n *Notifier) Notify() {
	n.mu.Lock()
	n.version++
	if n.changed != nil {
		close(n.changed)
	}
	n.changed = make(chan struct{})
	n.mu.Unlock()
}
