// Package secretbox 使用绑定调用方的 AEAD 加密按用途隔离的凭据。
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

// New 派生按用途隔离的加密密钥。解密已有密文须使用封装时相同的 purpose 和 AAD。
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

// 格式：版本字节（1）、随机 12 字节 nonce、密文与 GCM 标签。
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
