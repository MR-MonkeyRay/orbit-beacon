//go:build !linux

package probe

import (
	"context"
	"time"
)

func checkICMP(context.Context, string) (time.Duration, error) {
	return 0, errICMPUnsupported
}
