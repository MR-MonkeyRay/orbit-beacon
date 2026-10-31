package validation_test

import (
	"errors"
	"testing"

	"orbitbeacon/internal/server/validation"
)

func TestInvalid(t *testing.T) {
	err := validation.Invalid("name", "is required")
	var fields *validation.Error
	if !errors.As(err, &fields) {
		t.Fatalf("errors.As(%T) = false", err)
	}
	if got, want := fields.Error(), "Request values are invalid"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got, want := fields.Fields["name"], "is required"; got != want {
		t.Fatalf("Fields[name] = %q, want %q", got, want)
	}
}
