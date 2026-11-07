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
// 来源 ID（至多 35 字节、21 个来源）、标量指标、按最坏情况转义的 System、24 个流量桶、
// 完整能力枚举、2 条封闭指令结果及消息外围字段，以及 8 个磁盘各两个 uint64 inode 字段
// （每盘最多 71 字节，共 568 字节），以及新增磁盘 IO、PSI、温度字段（预留 1024 字节），
// 合计预留 15 KiB。探测结果的固定字段在下方另行预留。
// 其他上报实现仍须在发送前检查最终编码大小。
const reportEnvelopeBytes = 15 * 1024

// 为每个探测结果的固定字段预留空间；目标本身在下方按 encoding/json 的转义规则计入。
// tls_expires_at 另预留 56 字节，覆盖带纳秒及数字时区的最长时间编码。
const probeResultEnvelopeBytes = 216

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
