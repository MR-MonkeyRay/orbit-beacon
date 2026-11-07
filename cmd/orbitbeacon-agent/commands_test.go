package main

import (
	"testing"

	"orbitbeacon/internal/agent/service"
)

func TestCommandExitCodes(t *testing.T) {
	for _, code := range []int{0, 75} {
		if got := exitCode(&service.ExitError{Code: code}); got != code {
			t.Fatalf("got %d want %d", got, code)
		}
	}
}
