// Package backup 实现按类别划分的加密数据操作。
package backup

import "errors"

// Category 表示可独立导出、加密备份与恢复的数据类别标识。
type Category string

const (
	// Configuration 包含节点、分组、告警规则、系统设置等控制面配置数据。
	Configuration Category = "configuration"
	// Monitoring 包含时序指标历史、探测历史与流量聚合统计数据。
	Monitoring Category = "monitoring"
	// Events 包含审计日志、告警事件与通知历史等事件记录数据。
	Events Category = "events"
)

// ErrInvalidCategories 表示请求的备份类别为空、重复或包含不受支持的值。
var ErrInvalidCategories = errors.New("invalid backup categories")

// ValidateCategories 校验互相独立且不重复的类别，并按归档顺序返回。
// 仅选择数据类别时，不会自动加入 Configuration。
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
