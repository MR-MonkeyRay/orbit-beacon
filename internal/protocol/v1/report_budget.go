package v1

import (
	"errors"
	"unicode/utf8"
)

// MaxMessageBytes bounds agent requests and responses, including JSON escaping.
const MaxMessageBytes = 64 * 1024

// ErrReportBudget identifies a local selection exceeding the report wire limit.
var ErrReportBudget = errors.New("selection exceeds report encoding budget")

// Selected paths/names occur in series, two metadata keys and runtime. The
// collector's bounded ASCII source IDs (at most 35 bytes, 21 sources), scalar
// metrics, maximally escaped System, 24 traffic buckets and envelope fit 13 KiB.
// Probe result envelopes are reserved separately below. Arbitrary producers
// must still enforce the final encoded-message limit before sending.
const reportEnvelopeBytes = 13 * 1024

// Reserve the fixed fields around each encoded probe target. The target itself
// is accounted for below with encoding/json-compatible escaping.
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

// jsonStringBytes matches encoding/json's default HTML-safe string escaping
// without allocating an encoded copy of every configured series identifier.
func jsonStringBytes(value string) int {
	size := 2 // quotes
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
