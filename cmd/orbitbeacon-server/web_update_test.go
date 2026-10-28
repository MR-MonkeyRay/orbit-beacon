package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvalidBrowserCadenceDoesNotCreateServerData(t *testing.T) {
	for _, interval := range []string{"1", "61"} {
		t.Run(interval, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "not-created")
			err := run([]string{"serve", "--data-dir", path, "--public-url", "https://beacon.test", "--listen-addr", "127.0.0.1:0", "--web-update-interval-seconds", interval})
			if err == nil || !strings.Contains(err.Error(), "web_update_interval_seconds") {
				t.Fatalf("invalid cadence error=%v", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid configuration touched data directory: %v", err)
			}
		})
	}
}
