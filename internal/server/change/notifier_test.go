package change

import (
	"sync"
	"testing"
)

func TestNotifierZeroValueBroadcastsAndReplacesChannel(t *testing.T) {
	var notifier Notifier
	version, first := notifier.Snapshot()
	if version != 0 {
		t.Fatalf("initial version = %d, want 0", version)
	}
	_, listener := notifier.Snapshot()
	notifier.Notify()
	for name, changed := range map[string]<-chan struct{}{"first": first, "listener": listener} {
		select {
		case <-changed:
		default:
			t.Fatalf("notification did not wake %s snapshot", name)
		}
	}
	version, second := notifier.Snapshot()
	if version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}
	select {
	case <-second:
		t.Fatal("replacement channel is already closed")
	default:
	}
}

func TestNotifierConcurrentNotifyAndSnapshot(t *testing.T) {
	var notifier Notifier
	const notifications = 1000
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range notifications / 8 {
				notifier.Notify()
			}
		}()
	}
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range notifications {
				notifier.Snapshot()
			}
		}()
	}
	close(start)
	workers.Wait()
	version, _ := notifier.Snapshot()
	if version != notifications {
		t.Fatalf("version = %d, want %d", version, notifications)
	}
}
