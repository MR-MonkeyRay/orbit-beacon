package main

import (
	"errors"
	"testing"

	"orbitbeacon/internal/server/auth"
)

func TestRemovedBootstrapCommand(t *testing.T) {
	if err := run([]string{"bootstrap"}); err == nil {
		t.Fatalf("removed bootstrap accepted: %v", err)
	}
	if err := run([]string{"serve", "--username", "admin"}); err == nil {
		t.Fatal("removed username flag accepted")
	}
	if errors.Is(run([]string{"bootstrap"}), auth.ErrAdminExists) {
		t.Fatal("removed command entered authentication")
	}
}
