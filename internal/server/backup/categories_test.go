package backup

import (
	"reflect"
	"testing"
)

func TestCategoriesAreIndependentAndCanonical(t *testing.T) {
	all := []Category{Configuration, Monitoring, Events}
	for mask := 1; mask < 8; mask++ {
		var input, want []Category
		for i, c := range all {
			if mask&(1<<i) != 0 {
				want = append(want, c)
			}
		}
		for i := len(want) - 1; i >= 0; i-- {
			input = append(input, want[i])
		}
		got, err := ValidateCategories(input)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("mask=%d categories=%v err=%v", mask, got, err)
		}
	}
	for _, input := range [][]Category{nil, {}, {Configuration, Configuration}, {"unknown"}, {Events, Monitoring, Events}} {
		if _, err := ValidateCategories(input); err == nil {
			t.Fatalf("invalid categories accepted: %v", input)
		}
	}
}
