//go:build !linux

package services

import (
	"context"
	"net"
)

const pingSupported = false

func pingConn(context.Context, string, string) (net.Conn, error) { return nil, errUnsupported }
