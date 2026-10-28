//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func reloadSignals() (<-chan struct{}, func()) {
	signals := make(chan os.Signal, 1)
	reload := make(chan struct{}, 1)
	done := make(chan struct{})
	stopped := make(chan struct{})
	signal.Notify(signals, syscall.SIGHUP)
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			case <-signals:
				select {
				case reload <- struct{}{}:
				default:
				}
			}
		}
	}()
	return reload, func() { signal.Stop(signals); close(done); <-stopped }
}
