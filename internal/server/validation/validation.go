// Package validation 定义请求字段级校验错误类型。
package validation

// Error 报告校验失败的请求字段。
type Error struct{ Fields map[string]string }

// Error 实现 error 接口，返回固定的通用错误描述以避免泄露具体字段值。
func (err *Error) Error() string { return "Request values are invalid" }

// Invalid 返回单个请求字段的校验错误。
func Invalid(field, message string) error {
	return &Error{Fields: map[string]string{field: message}}
}
