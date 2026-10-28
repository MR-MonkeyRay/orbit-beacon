package probe

import (
	"encoding/binary"
	"errors"
)

var (
	errICMPUnsupported = errors.New("ICMP probing is unsupported")
	errInvalidICMP     = errors.New("invalid ICMP packet")
)

type icmpUnsupportedError struct{ error }

func (icmpUnsupportedError) Unwrap() error { return errICMPUnsupported }

func buildICMPEchoRequest(ipv6 bool, sequence uint16) []byte {
	packet := make([]byte, 8)
	if ipv6 {
		packet[0] = 128
	} else {
		packet[0] = 8
	}
	binary.BigEndian.PutUint16(packet[6:8], sequence)
	if !ipv6 {
		binary.BigEndian.PutUint16(packet[2:4], internetChecksum(packet))
	}
	return packet
}

func parseICMPEchoReply(packet []byte, ipv6 bool, sequence uint16) error {
	if len(packet) < 8 {
		return errInvalidICMP
	}
	wantType := byte(0)
	if ipv6 {
		wantType = 129
	}
	if packet[0] != wantType || packet[1] != 0 || binary.BigEndian.Uint16(packet[6:8]) != sequence {
		return errInvalidICMP
	}
	return nil
}

func internetChecksum(data []byte) uint16 {
	var sum uint32
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data[:2]))
		data = data[2:]
	}
	if len(data) != 0 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
