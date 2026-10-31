// SPDX-License-Identifier: GPL-3.0-or-later
//go:build linux

package collect

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func procPath(parts ...string) string {
	root := os.Getenv("HOST_PROC")
	if root == "" {
		root = "/proc"
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

func uptimeValue(ctx context.Context) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	file, err := os.Open(procPath("uptime"))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 4096))
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return 0, err
		}
		return 0, io.ErrUnexpectedEOF
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	field := fields[0]
	if !strings.ContainsAny(field, ".eE") {
		integer, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, errors.New("invalid proc uptime")
		}
		return float64(integer), nil
	}
	seconds, err := strconv.ParseFloat(field, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds >= math.Ldexp(1, 64) {
		return 0, errors.New("invalid proc uptime")
	}
	return seconds, nil
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
	if root := os.Getenv("HOST_SYS"); root != "" {
		if name == "" || len(name) > 15 || name == "." || strings.Contains(name, "..") || strings.ContainsAny(name, "/\\\x00: \t\r\n") {
			return false
		}
		// sysfs 网卡符号链接可能指向 devices/，但必须留在 HOST_SYS 内。
		file, err := os.OpenInRoot(root, filepath.Join("class", "net", name, "flags"))
		if err != nil {
			return false
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 64))
		if err != nil {
			return false
		}
		flags, err := strconv.ParseUint(strings.TrimSpace(string(data)), 0, 32)
		return err == nil && flags&syscall.IFF_UP != 0 && flags&syscall.IFF_LOOPBACK == 0
	}
	iface, err := net.InterfaceByName(name)
	return err == nil && iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0
}

func networkProcPath(name string) string {
	// 即使挂载的是宿主机 proc，/proc/net 仍指向 self/net。
	// 从 PID 1 读取路由和计数，确保两者都来自宿主机 init 的网络命名空间。
	if os.Getenv("HOST_PROC") != "" {
		return procPath("1", "net", name)
	}
	return procPath("net", name)
}

func defaultInterface(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.Open(networkProcPath("route"))
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
	// 纯 IPv6 主机需要真实的默认路由，不能随便选一个网卡。
	v6, err := os.Open(networkProcPath("ipv6_route"))
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
