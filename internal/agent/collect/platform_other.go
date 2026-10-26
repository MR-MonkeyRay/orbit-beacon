// SPDX-License-Identifier: GPL-3.0-or-later
//go:build !linux && !darwin

package collect

import (
	"context"

	"github.com/shirou/gopsutil/v4/load"
)

func uptimeSourceAvailable() error { return nil }

func loadAverage(ctx context.Context) (loadValues, error) {
	value, err := load.AvgWithContext(ctx)
	if err != nil {
		return loadValues{}, err
	}
	return loadValues{one: value.Load1, five: value.Load5, fifteen: value.Load15}, nil
}

func defaultInterface(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", noDefaultInterface()
}
