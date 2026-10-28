package traffic

import (
	"math"
	"testing"
)

func TestSaturatingAdd(t *testing.T) {
	tests := []struct {
		name        string
		left, right int64
		want        int64
	}{
		{name: "zero", want: 0},
		{name: "ordinary", left: 12, right: 34, want: 46},
		{name: "exact maximum", left: math.MaxInt64 - 1, right: 1, want: math.MaxInt64},
		{name: "overflow", left: math.MaxInt64, right: math.MaxInt64, want: math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := saturatingAdd(tt.left, tt.right); got != tt.want {
				t.Fatalf("saturatingAdd(%d, %d) = %d, want %d", tt.left, tt.right, got, tt.want)
			}
		})
	}
}
