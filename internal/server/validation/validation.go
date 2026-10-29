package validation

// Error reports request fields that failed validation.
type Error struct{ Fields map[string]string }

func (err *Error) Error() string { return "Request values are invalid" }

// Invalid returns a validation error for one request field.
func Invalid(field, message string) error {
	return &Error{Fields: map[string]string{field: message}}
}
