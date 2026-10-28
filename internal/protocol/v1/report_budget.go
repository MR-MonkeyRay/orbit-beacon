package v1

import (
	"errors"
	"unicode/utf8"
)

// MaxMessageBytes bounds agent requests and responses, including JSON escaping.
const MaxMessageBytes = 64 * 1024

// ErrReportBudget identifies a formerly valid saved selection that needs a
// bounded diagnostic report before a higher-revision remote repair can arrive.
var ErrReportBudget = errors.New("selection exceeds report encoding budget")

// The collector repeats each selected path/name in its series and two metadata
// keys. Reserve 16 KiB for the bounded scalar metrics, metadata, system identity,
// configuration errors, up to MaxTrafficEntries traffic buckets, and envelope.
// This is a configuration admission bound, not permission for clients to omit
// the final encoded-message size check.
const reportEnvelopeBytes = 16 * 1024

// Reserve the fixed fields around each encoded probe target. The target itself
// is accounted for below with encoding/json-compatible escaping.
const probeResultEnvelopeBytes = 160

func selectionFitsReport(c DesiredConfig) bool {
	bytes := 0
	for _, path := range c.DiskPaths {
		bytes += jsonStringBytes(path)
	}
	for _, name := range c.Interfaces {
		bytes += jsonStringBytes(name)
	}
	probeBytes := 0
	for _, target := range c.ProbeTargets {
		probeBytes += jsonStringBytes(target) + probeResultEnvelopeBytes
	}
	return 3*bytes+probeBytes+reportEnvelopeBytes <= MaxMessageBytes
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
