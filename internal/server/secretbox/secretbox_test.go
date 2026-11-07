package secretbox

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

func TestChannelBoundCiphertext(t *testing.T) {
	b, e := New(bytes.Repeat([]byte{7}, 32), "orbitbeacon notification channel v1")
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
	other, _ := New(bytes.Repeat([]byte{8}, 32), "orbitbeacon notification channel v1")
	if _, e = other.Open("channel-a", a); e == nil {
		t.Fatal("wrong key accepted")
	}
	for _, raw := range [][]byte{nil, {1}, a[:12]} {
		if _, e = b.Open("channel-a", raw); e == nil {
			t.Fatal("truncated ciphertext accepted")
		}
	}
	if _, e = New([]byte("short"), "orbitbeacon notification channel v1"); e == nil {
		t.Fatal("short key accepted")
	}
}

func TestLegacyNotificationCiphertext(t *testing.T) {
	// 用途隔离之前的固定格式：HKDF-SHA256 通知密钥、nonce 00..0b、
	// 版本 1，以通道 ID 作为 AAD。此测试数据绝不能用 New/Seal 重新生成。
	raw, err := hex.DecodeString("01000102030405060708090a0bae37bd2f4705a08aac0bd5dfaca29029e05238f91500c2cbddf32b1a2ff4de4280407962de7a7d5ef5f685961aec")
	if err != nil {
		t.Fatal(err)
	}
	box, err := New(bytes.Repeat([]byte{7}, 32), "orbitbeacon notification channel v1")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := box.Open("channel-legacy", raw)
	if err != nil || string(plain) != "legacy-notification-credential" {
		t.Fatalf("legacy notification ciphertext unreadable: %q, %v", plain, err)
	}
}

func TestPurposeAndAADSeparation(t *testing.T) {
	master := bytes.Repeat([]byte{7}, 32)
	notification, err := New(master, "orbitbeacon notification channel v1")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := New(master, "orbitbeacon backup secrets v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []*Box{notification, backup} {
		other := notification
		if source == notification {
			other = backup
		}
		for _, aad := range []string{"plan:id", "task-input:id", "task-password:id"} {
			raw, err := source.Seal(aad, []byte("credential"))
			if err != nil {
				t.Fatal(err)
			}
			plain, err := source.Open(aad, raw)
			if err != nil || string(plain) != "credential" {
				t.Fatalf("roundtrip for %s: %q, %v", aad, plain, err)
			}
			if _, err := other.Open(aad, raw); !errors.Is(err, ErrCiphertext) {
				t.Fatalf("cross-purpose ciphertext accepted: %v", err)
			}
			for _, otherAAD := range []string{"plan:other", "task-input:id", "task-password:id"} {
				if otherAAD != aad {
					if _, err := source.Open(otherAAD, raw); !errors.Is(err, ErrCiphertext) {
						t.Fatalf("AAD substitution %s -> %s accepted: %v", aad, otherAAD, err)
					}
				}
			}
		}
	}
}
