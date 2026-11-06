package main

import (
	"strings"
	"testing"
)

func TestVictoriaMetricsURLFlagAndNoTokenFlag(t *testing.T) {
	err := run([]string{"serve", "--history-victoriametrics-url", "ftp://vm.test"})
	if err == nil || !strings.Contains(err.Error(), "history_victoriametrics_url") {
		t.Fatalf("URL flag error=%v", err)
	}
	err = run([]string{"serve", "--history-victoriametrics-token", "private-token"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("token flag error=%v", err)
	}
}
