package v1

import (
	"errors"
	"unicode/utf8"
)

// MaxMessageBytes 限定 Agent 请求与响应的编码字节数，含 JSON 转义。
const MaxMessageBytes = 64 * 1024

// ErrReportBudget 表示本地采集配置超出了上报编码大小限制。
var ErrReportBudget = errors.New("selection exceeds report encoding budget")

// 选定的路径和名称会出现在指标序列、两个元数据键和运行时配置中。采集器的 ASCII
// 来源 ID（至多 35 字节、21 个来源）、标量指标、按最坏情况转义的 System、24 个流量桶
// 及消息外围字段合计不超过 13 KiB。探测结果的固定字段在下方另行预留。
// 其他上报实现仍须在发送前检查最终编码大小。
const reportEnvelopeBytes = 13 * 1024

// 为每个探测结果的固定字段预留空间；目标本身在下方按 encoding/json 的转义规则计入。
const probeResultEnvelopeBytes = 160

func selectionFitsReport(c RuntimeSettings) bool {
	bytes := 0
	for _, path := range c.DiskPaths {
		bytes += jsonStringBytes(path)
	}
	for _, name := range c.Interfaces {
		bytes += jsonStringBytes(name)
	}
	probeBytes := 0
	for _, target := range c.ProbeTargets {
		probeBytes += 2*jsonStringBytes(target) + probeResultEnvelopeBytes
	}
	return 4*bytes+probeBytes+reportEnvelopeBytes <= MaxMessageBytes
}

// jsonStringBytes 与 encoding/json 默认的 HTML 安全字符串转义一致，
// 且无需为每个配置的序列标识符分配编码副本。
func jsonStringBytes(value string) int {
	size := 2 // 引号
	for i := 0; i < len(value); {
		b := value[i]
		if b < utf8.RuneSelf {
			switch b {
			case '"', '\\', '\b', '\f', '\n', '\r', '\t':
				size += 2
			case '<', '>', '&':
				size += 6
			default:
				if b < 0x20 {
					size += 6
				} else {
					size++
				}
			}
			i++
			continue
		}
		r, width := utf8.DecodeRuneInString(value[i:])
		if (r == utf8.RuneError && width == 1) || r == '\u2028' || r == '\u2029' {
			size += 6
		} else {
			size += width
		}
		i += width
	}
	return size
}
