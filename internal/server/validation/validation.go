package validation

// Error 报告校验失败的请求字段。
type Error struct{ Fields map[string]string }

func (err *Error) Error() string { return "Request values are invalid" }

// Invalid 返回单个请求字段的校验错误。
func Invalid(field, message string) error {
	return &Error{Fields: map[string]string{field: message}}
}
