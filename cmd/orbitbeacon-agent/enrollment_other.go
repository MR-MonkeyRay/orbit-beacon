//go:build !linux

package main

import "errors"

type enrollmentTicket struct {
	token string
}

func openEnrollmentTicket(string) (*enrollmentTicket, error) {
	return nil, errors.New("secure enrollment token handling is unsupported on this platform")
}

func (*enrollmentTicket) close() {}

func (*enrollmentTicket) remove() error {
	return errors.New("secure enrollment token cleanup is unsupported on this platform")
}
