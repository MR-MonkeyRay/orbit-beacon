// Package secretbox encrypts purpose-separated credentials with caller-bound AEAD.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

var ErrCiphertext = errors.New("invalid secret ciphertext")

type Box struct{ aead cipher.AEAD }

// New derives an encryption key scoped to purpose. Existing ciphertext requires
// the same purpose and AAD that were used when it was sealed.
func New(master []byte, purpose string) (*Box, error) {
	if len(master) != 32 {
		return nil, errors.New("secret key must contain 32 bytes")
	}
	key, err := hkdf.Key(sha256.New, master, nil, purpose, 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Format: version byte (1), random 12-byte nonce, ciphertext and GCM tag.
func (b *Box) Seal(aad string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	prefix := append([]byte{1}, nonce...)
	return b.aead.Seal(prefix, nonce, plaintext, []byte(aad)), nil
}
func (b *Box) Open(aad string, ciphertext []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(ciphertext) < 1+n+b.aead.Overhead() || ciphertext[0] != 1 {
		return nil, ErrCiphertext
	}
	plain, err := b.aead.Open(nil, ciphertext[1:1+n], ciphertext[1+n:], []byte(aad))
	if err != nil {
		return nil, ErrCiphertext
	}
	return plain, nil
}
