// Package backup implements encrypted, category-scoped data operations.
package backup

import "errors"

type Category string

const (
	Configuration Category = "configuration"
	Monitoring    Category = "monitoring"
	Events        Category = "events"
)

var ErrInvalidCategories = errors.New("invalid backup categories")

// ValidateCategories accepts independent, unique categories and returns archive
// order. Configuration is never added implicitly to a data-only selection.
func ValidateCategories(input []Category) ([]Category, error) {
	if len(input) < 1 || len(input) > 3 {
		return nil, ErrInvalidCategories
	}
	var selected uint8
	for _, category := range input {
		var bit uint8
		switch category {
		case Configuration:
			bit = 1
		case Monitoring:
			bit = 2
		case Events:
			bit = 4
		default:
			return nil, ErrInvalidCategories
		}
		if selected&bit != 0 {
			return nil, ErrInvalidCategories
		}
		selected |= bit
	}
	result := make([]Category, 0, len(input))
	for i, category := range [...]Category{Configuration, Monitoring, Events} {
		if selected&(1<<i) != 0 {
			result = append(result, category)
		}
	}
	return result, nil
}
