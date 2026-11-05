package ingest

import (
	"context"
	"testing"
)

func TestWriteQueueDepth(t *testing.T) {
	s := NewService(nil, Options{})
	if s.WriteQueueDepth() != 0 {
		t.Fatal("new queue not empty")
	}
	release, err := s.queueWrite(context.Background(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	if s.WriteQueueDepth() != 1 {
		t.Fatal("active writer not included")
	}
	release()
	if s.WriteQueueDepth() != 0 {
		t.Fatal("released slot still counted")
	}
}
