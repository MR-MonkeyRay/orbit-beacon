package change

import "sync"

// Notifier broadcasts changes by closing the current channel.
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
