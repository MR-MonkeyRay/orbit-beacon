//go:build !linux

package main

import "errors"

type enrollmentTicket struct {
	token string
}

func openEnrollmentTicket(string) (*enrollmentTicket, error) {
	return nil, errors.New("本平台不支持安全处理注册票据")
}

func (*enrollmentTicket) close() {}

func (*enrollmentTicket) remove() error {
	return errors.New("本平台不支持安全清理注册票据")
}
