package config

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"gopkg.in/yaml.v3"
)

// decodeYAML 保留 JSON 字段标签和自定义 JSON 校验，不维护第二套配置结构。
func decodeYAML(reader io.Reader) ([]byte, error) {
	decoder := yaml.NewDecoder(reader)
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("YAML 配置结构无效")
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("YAML 配置必须是字符串键映射")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("YAML 配置只能包含一个文档")
	}
	data, err := json.Marshal(yamlJSONNumbers(value))
	if err != nil {
		return nil, errors.New("YAML 配置包含非字符串映射键或不支持的值")
	}
	return data, nil
}

func yamlJSONNumbers(value any) any {
	switch value := value.(type) {
	case float64:
		// 保留浮点类型，防止 10.0 在 JSON 桥接后被整数字段接受。
		return json.Number(strconv.FormatFloat(value, 'e', -1, 64))
	case map[string]any:
		for key, item := range value {
			value[key] = yamlJSONNumbers(item)
		}
	case []any:
		for index, item := range value {
			value[index] = yamlJSONNumbers(item)
		}
	}
	return value
}
