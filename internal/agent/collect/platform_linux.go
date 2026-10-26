// SPDX-License-Identifier: GPL-3.0-or-later
//go:build linux

package collect

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func procPath(parts ...string) string {
	root := os.Getenv("HOST_PROC")
	if root == "" {
		root = "/proc"
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// gopsutil's Linux uptime reader uses sysinfo rather than procfs. Require the
// configured host source to be available as well; otherwise a remapped or
// inaccessible host procfs would silently report the collector host's uptime.
func uptimeSourceAvailable() error {
	file, err := os.Open(procPath("uptime"))
	if err != nil {
		return err
	}
	defer file.Close()
	var first [1]byte
	_, err = io.ReadFull(file, first[:])
	return err
}

func loadAverage(ctx context.Context) (loadValues, error) {
	if err := ctx.Err(); err != nil {
		return loadValues{}, err
	}
	file, err := os.Open(procPath("loadavg"))
	if err != nil {
		return loadValues{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 4096))
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return loadValues{}, err
		}
		return loadValues{}, io.ErrUnexpectedEOF
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 3 {
		return loadValues{}, io.ErrUnexpectedEOF
	}
	values := loadValues{}
	for index, target := range []*float64{&values.one, &values.five, &values.fifteen} {
		parsed, err := strconv.ParseFloat(fields[index], 64)
		if err != nil {
			return loadValues{}, err
		}
		*target = parsed
	}
	return values, nil
}

func usableInterface(name string) bool {
	iface, err := net.InterfaceByName(name)
	return err == nil && iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0
}

func defaultInterface(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.Open(procPath("net", "route"))
	if err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(io.LimitReader(file, 1<<20))
		var best string
		var bestMetric uint64
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			fields := strings.Fields(scanner.Text())
			if len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
				continue
			}
			flags, flagErr := strconv.ParseUint(fields[3], 16, 32)
			metric, metricErr := strconv.ParseUint(fields[6], 10, 64)
			if flagErr != nil || metricErr != nil || flags&1 == 0 || !usableInterface(fields[0]) {
				continue
			}
			if best == "" || metric < bestMetric {
				best, bestMetric = fields[0], metric
			}
		}
		if best != "" {
			return best, nil
		}
	}
	// IPv6-only hosts need their actual default route, not an arbitrary NIC.
	v6, err := os.Open(procPath("net", "ipv6_route"))
	if err != nil {
		return "", noDefaultInterface()
	}
	defer v6.Close()
	scanner := bufio.NewScanner(io.LimitReader(v6, 1<<20))
	var best string
	var bestMetric uint64
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || fields[0] != "00000000000000000000000000000000" || fields[1] != "00" {
			continue
		}
		metric, metricErr := strconv.ParseUint(fields[5], 16, 64)
		flags, flagErr := strconv.ParseUint(fields[8], 16, 32)
		if metricErr != nil || flagErr != nil || flags&1 == 0 || !usableInterface(fields[9]) {
			continue
		}
		if best == "" || metric < bestMetric {
			best, bestMetric = fields[9], metric
		}
	}
	if best == "" {
		return "", noDefaultInterface()
	}
	return best, nil
}
