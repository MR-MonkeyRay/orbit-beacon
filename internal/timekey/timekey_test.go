package timekey

import (
	"testing"
	"time"
)

func TestFormatIsUTCAndChronologicallyComparable(t *testing.T) {
	first := Format(time.Date(2026, 10, 26, 12, 0, 0, 123000000, time.UTC))
	second := Format(time.Date(2026, 10, 26, 14, 0, 1, 0, time.FixedZone("offset", 2*60*60)))
	if first >= second {
		t.Fatalf("encoded times are not chronologically ordered: %q >= %q", first, second)
	}
	lower := Format(time.Date(2026, 10, 26, 12, 0, 0, 123456780, time.UTC))
	higher := Format(time.Date(2026, 10, 26, 12, 0, 0, 123456789, time.UTC))
	if lower >= higher {
		t.Fatalf("same-second fractions are not ordered: %q >= %q", lower, higher)
	}
	parsed, err := Parse("2026-10-26T14:00:00.123+02:00")
	if err != nil {
		t.Fatal(err)
	}
	if got := Format(parsed); got != first {
		t.Fatalf("normalized offset=%q, want %q", got, first)
	}
}
