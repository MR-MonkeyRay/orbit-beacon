// SPDX-License-Identifier: GPL-3.0-or-later
//go:build linux

package collect

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func writeNetworkFixture(t *testing.T, root, name, value string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxHostSysInterfaceFlags(t *testing.T) {
	sys := t.TempDir()
	t.Setenv("HOST_SYS", sys)
	for _, scenario := range []struct {
		name, flags string
		want        bool
	}{
		{"host0", "0x1003\n", true},
		{"down0", "0x1002\n", false},
		{"loop0", "0x9\n", false},
		{"invalid0", "invalid\n", false},
		{"negative0", "-1\n", false},
		{"overflow0", "0x100000001\n", false},
		{"abcdefghijklmno", "0x1\n", true},
		{"abcdefghijklmnop", "0x1\n", false},
		{"nic..0", "0x1\n", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			writeNetworkFixture(t, sys, "class/net/"+scenario.name+"/flags", scenario.flags)
			if got := usableInterface(scenario.name); got != scenario.want {
				t.Fatalf("usableInterface(%q)=%v want %v", scenario.name, got, scenario.want)
			}
		})
	}
	// Even a valid local NIC must not bypass the selected host sysfs view.
	if usableInterface("lo") || usableInterface("missing0") {
		t.Fatal("missing host interface used the process network namespace")
	}
	for _, name := range []string{"", ".", "..", "../host0", "host0/", "host0/../host0", "host0\\flags", "host0\x00", "host 0"} {
		if usableInterface(name) {
			t.Errorf("accepted invalid interface name %q", name)
		}
	}
	outside := t.TempDir()
	writeNetworkFixture(t, outside, "flags", "0x1\n")
	if err := os.Symlink(outside, filepath.Join(sys, "class/net/escape0")); err != nil {
		t.Fatal(err)
	}
	if usableInterface("escape0") {
		t.Fatal("interface symlink escaped HOST_SYS")
	}
	writeNetworkFixture(t, sys, "devices/virtual/net/link0/flags", "0x1\n")
	if err := os.Symlink("../../devices/virtual/net/link0", filepath.Join(sys, "class/net/link0")); err != nil {
		t.Fatal(err)
	}
	if !usableInterface("link0") {
		t.Fatal("rejected normal sysfs symlink within HOST_SYS")
	}
}

func TestLinuxHostDefaultInterfaceUsesInitNamespace(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		t.Run(fmt.Sprintf("ipv6=%v", ipv6), func(t *testing.T) {
			proc, sys := t.TempDir(), t.TempDir()
			t.Setenv("HOST_PROC", proc)
			t.Setenv("HOST_SYS", sys)
			writeNetworkFixture(t, sys, "class/net/host0/flags", "0x1\n")
			writeNetworkFixture(t, sys, "class/net/slow0/flags", "0x1\n")
			writeNetworkFixture(t, sys, "class/net/local0/flags", "0x1\n")
			writeNetworkFixture(t, sys, "class/net/down0/flags", "0x0\n")
			writeNetworkFixture(t, sys, "class/net/loop0/flags", "0x9\n")
			writeNetworkFixture(t, proc, "net/route", "local0 00000000 00000000 0001 0 0 0 00000000\n")
			if ipv6 {
				row := "00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 %08x 00000000 00000000 00000001 %s\n"
				writeNetworkFixture(t, proc, "1/net/ipv6_route", fmt.Sprintf(row, 0, "down0")+fmt.Sprintf(row, 0, "loop0")+fmt.Sprintf(row, 20, "slow0")+fmt.Sprintf(row, 10, "host0"))
			} else {
				row := "%s 00000000 00000000 0001 0 0 %d 00000000\n"
				writeNetworkFixture(t, proc, "1/net/route", fmt.Sprintf(row, "../host0", 0)+fmt.Sprintf(row, "down0", 0)+fmt.Sprintf(row, "loop0", 0)+fmt.Sprintf(row, "slow0", 20)+fmt.Sprintf(row, "host0", 10))
			}
			name, err := defaultInterface(context.Background())
			if err != nil || name != "host0" {
				t.Fatalf("defaultInterface()=%q, %v want host0", name, err)
			}
		})
	}
}

func TestLinuxHostNetworkRatesUseInitCounters(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%v", explicit), func(t *testing.T) {
			proc, sys := t.TempDir(), t.TempDir()
			t.Setenv("HOST_PROC", proc)
			t.Setenv("HOST_SYS", sys)
			writeNetworkFixture(t, sys, "class/net/host0/flags", "0x1\n")
			writeNetworkFixture(t, proc, "1/net/route", "host0 00000000 00000000 0001 0 0 0 00000000\n")
			dev := func(rx, tx int) string {
				return fmt.Sprintf("Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n host0: %d 0 0 0 0 0 0 0 %d 0 0 0 0 0 0 0\n", rx, tx)
			}
			writeNetworkFixture(t, proc, "net/dev", dev(900000, 900000))
			writeNetworkFixture(t, proc, "self/net/dev", dev(800000, 800000))
			writeNetworkFixture(t, proc, "1/net/dev", dev(100, 200))
			synctest.Test(t, func(t *testing.T) {
				config := v1.DesiredConfig{Revision: 1, ReportIntervalSeconds: 5, DiskIntervalSeconds: 60, BillingDay: 1, QuotaBytes: "0"}
				if explicit {
					config.Interfaces = []string{"host0"}
				}
				collector, err := New(Options{Config: config})
				if err != nil {
					t.Fatal(err)
				}
				read := func() v1.Metrics {
					t.Helper()
					metrics := v1.Metrics{MetricStatus: make(map[string]string), Sources: make(map[string]v1.SourceMetadata)}
					collector.collectNetworks(context.Background(), &metrics)
					return metrics
				}
				first := read()
				if len(first.Networks) != 1 || first.Networks[0].Name != "host0" || first.MetricStatus["network:host0"] != "stale" {
					t.Fatalf("missing host baseline: %+v", first)
				}
				time.Sleep(time.Second)
				writeNetworkFixture(t, proc, "1/net/dev", dev(400, 700))
				second := read()
				if len(second.Networks) != 1 || second.MetricStatus["network:host0"] != "ok" {
					t.Fatalf("missing host rate: %+v", second)
				}
				traffic := collector.Traffic().Entries()
				if len(traffic) != 1 || traffic[0].RxBytes != "300" || traffic[0].TxBytes != "500" {
					t.Fatalf("traffic did not reuse selected PID 1 counters: %+v", traffic)
				}
				network := second.Networks[0]
				if network.RxBytesPerSec == nil || *network.RxBytesPerSec != 300 || network.TxBytesPerSec == nil || *network.TxBytesPerSec != 500 {
					t.Fatalf("rates did not use PID 1 counters: %+v", network)
				}
				if err := os.Remove(filepath.Join(proc, "1/net/dev")); err != nil {
					t.Fatal(err)
				}
				failed := read()
				if failed.MetricStatus["network:host0"] != "error" || failed.Networks[0].RxBytesPerSec != nil {
					t.Fatalf("missing host counters fell back to process namespace: %+v", failed)
				}
			})
		})
	}
}

func TestLinuxHostNetworkMissingInitRouteDoesNotUseSelf(t *testing.T) {
	proc, sys := t.TempDir(), t.TempDir()
	t.Setenv("HOST_PROC", proc)
	t.Setenv("HOST_SYS", sys)
	writeNetworkFixture(t, sys, "class/net/local0/flags", "0x1\n")
	writeNetworkFixture(t, proc, "net/route", "local0 00000000 00000000 0001 0 0 0 00000000\n")
	if name, err := defaultInterface(context.Background()); err == nil || name != "" {
		t.Fatalf("missing host route used process namespace: %q, %v", name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := defaultInterface(ctx); err != context.Canceled {
		t.Fatalf("canceled route lookup=%v", err)
	}
}
