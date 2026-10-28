package secretbox

import (
	"bytes"
	"testing"
)

func TestChannelBoundCiphertext(t *testing.T) {
	b, e := New(bytes.Repeat([]byte{7}, 32))
	if e != nil {
		t.Fatal(e)
	}
	a, e := b.Seal("channel-a", []byte("credential"))
	if e != nil {
		t.Fatal(e)
	}
	second, _ := b.Seal("channel-a", []byte("credential"))
	if bytes.Equal(a, second) {
		t.Fatal("nonce reused")
	}
	plain, e := b.Open("channel-a", a)
	if e != nil || string(plain) != "credential" {
		t.Fatalf("roundtrip %v", e)
	}
	if _, e = b.Open("channel-b", a); e == nil {
		t.Fatal("channel substitution accepted")
	}
	for _, i := range []int{0, 1, len(a) - 1} {
		bad := bytes.Clone(a)
		bad[i] ^= 1
		if _, e = b.Open("channel-a", bad); e == nil {
			t.Fatal("tampering accepted")
		}
	}
	other, _ := New(bytes.Repeat([]byte{8}, 32))
	if _, e = other.Open("channel-a", a); e == nil {
		t.Fatal("wrong key accepted")
	}
	for _, raw := range [][]byte{nil, {1}, a[:12]} {
		if _, e = b.Open("channel-a", raw); e == nil {
			t.Fatal("truncated ciphertext accepted")
		}
	}
	if _, e = New([]byte("short")); e == nil {
		t.Fatal("short key accepted")
	}
}
