//go:build windows

package main

func reloadSignals() (<-chan struct{}, func()) { return nil, func() {} }
