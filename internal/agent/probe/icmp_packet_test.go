package probe

import (
	"encoding/binary"
	"testing"
)

func TestBuildICMPEchoRequest(t *testing.T) {
	const sequence = 0x1234
	packet := buildICMPEchoRequest(false, sequence)
	if len(packet) != 8 || packet[0] != 8 || packet[1] != 0 {
		t.Fatalf("unexpected IPv4 echo request: %x", packet)
	}
	if got := binary.BigEndian.Uint16(packet[6:8]); got != sequence {
		t.Fatalf("sequence = %#x, want %#x", got, sequence)
	}
	if checksum := internetChecksum(packet); checksum != 0 {
		t.Fatalf("checksum over complete packet = %#x, want 0", checksum)
	}

	packet = buildICMPEchoRequest(true, sequence)
	if len(packet) != 8 || packet[0] != 128 || packet[1] != 0 {
		t.Fatalf("unexpected IPv6 echo request: %x", packet)
	}
	if got := binary.BigEndian.Uint16(packet[6:8]); got != sequence {
		t.Fatalf("sequence = %#x, want %#x", got, sequence)
	}
}

func TestParseICMPEchoReply(t *testing.T) {
	const sequence = 7
	for _, test := range []struct {
		name    string
		packet  []byte
		ipv6    bool
		wantErr bool
	}{
		{name: "IPv4", packet: []byte{0, 0, 0, 0, 0, 0, 0, sequence}},
		{name: "IPv6", packet: []byte{129, 0, 0, 0, 0, 0, 0, sequence}, ipv6: true},
		{name: "short", packet: []byte{0, 0}, wantErr: true},
		{name: "wrong type", packet: []byte{8, 0, 0, 0, 0, 0, 0, sequence}, wantErr: true},
		{name: "wrong code", packet: []byte{0, 1, 0, 0, 0, 0, 0, sequence}, wantErr: true},
		{name: "wrong sequence", packet: []byte{0, 0, 0, 0, 0, 0, 0, sequence + 1}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := parseICMPEchoReply(test.packet, test.ipv6, sequence)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseICMPEchoReply() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestInternetChecksumOddLength(t *testing.T) {
	if got, want := internetChecksum([]byte{1, 2, 3}), uint16(0xfbfd); got != want {
		t.Fatalf("internetChecksum() = %#x, want %#x", got, want)
	}
}
