package uuid_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"orbitbeacon/internal/uuid"
)

func TestUUID(t *testing.T) {
	value, err := uuid.UUID(bytes.NewReader([]byte{
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if value != "00112233-4455-4677-8899-aabbccddeeff" {
		t.Fatalf("UUID() = %q", value)
	}
}

func TestUUIDReadError(t *testing.T) {
	_, err := uuid.UUID(bytes.NewReader(make([]byte, 15)))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("UUID() error = %v, want unexpected EOF", err)
	}
}

func TestValidUUID(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "lowercase", value: "00112233-4455-4677-8899-aabbccddeeff", valid: true},
		{name: "uppercase", value: "00112233-4455-4677-8899-AABBCCDDEEFF", valid: true},
		{name: "version one", value: "00112233-4455-1677-8899-aabbccddeeff", valid: true},
		{name: "version eight", value: "00112233-4455-8677-B899-aabbccddeeff", valid: true},
		{name: "wrong length", value: "00112233-4455-4677-8899-aabbccddee", valid: false},
		{name: "wrong separators", value: "001122334455-4677-8899-aabbccddeeff", valid: false},
		{name: "non hexadecimal", value: "00112233-4455-4677-8899-aabbccddeefg", valid: false},
		{name: "nil version", value: "00112233-4455-0677-8899-aabbccddeeff", valid: false},
		{name: "version nine", value: "00112233-4455-9677-8899-aabbccddeeff", valid: false},
		{name: "reserved variant", value: "00112233-4455-4677-c899-aabbccddeeff", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := uuid.ValidUUID(test.value); got != test.valid {
				t.Fatalf("ValidUUID(%q) = %t, want %t", test.value, got, test.valid)
			}
		})
	}
}
