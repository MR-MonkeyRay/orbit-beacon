// Package uuid 提供无外部依赖的 UUID v4 生成与格式校验。
package uuid

import (
	"encoding/hex"
	"io"
)

// UUID 从 random 读取 16 字节随机熵，生成标准带连字符的 RFC 4122 v4 字符串。
func UUID(random io.Reader) (string, error) {
	var bytes [16]byte
	if _, err := io.ReadFull(random, bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	var encoded [36]byte
	hex.Encode(encoded[0:8], bytes[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], bytes[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], bytes[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], bytes[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], bytes[10:16])
	return string(encoded[:]), nil
}

// ValidUUID 校验 value 是否为标准 36 字符且包含有效版本与变体位的 UUID。
func ValidUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := range len(value) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return false
			}
			continue
		}
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return value[14] >= '1' && value[14] <= '8' && (value[19] == '8' || value[19] == '9' || value[19] == 'a' || value[19] == 'b' || value[19] == 'A' || value[19] == 'B')
}
