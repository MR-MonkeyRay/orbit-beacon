// Package timekey 统一数据库持久化时间戳的 UTC 格式与字典序编码。
package timekey

import "time"

// persistedLayout 固定 9 位小数秒与 UTC 时区，确保文本排序与时间先后一致。
const persistedLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Format 统一持久化时间格式，使用 UTC 与固定纳秒精度，
// 使 SQLite 的字节序与时间顺序一致。
func Format(value time.Time) string {
	return value.UTC().Format(persistedLayout)
}

// Parse 读取持久化时间并转换为 UTC。
func Parse(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}
