package utils

import (
	"strings"
)

// SanitizeLabelValue 清理标签值
func SanitizeLabelValue(value string) string {
	// 移除或替换不安全的字符
	value = strings.ReplaceAll(value, "\"", "")
	value = strings.ReplaceAll(value, "\\", "")
	value = strings.ReplaceAll(value, "\n", "_")
	return value
}

// ParseLabels 解析标签字符串
func ParseLabels(s string) map[string]string {
	labels := make(map[string]string)
	if s == "" {
		return labels
	}

	pairs := strings.Split(s, ",")
	for _, pair := range pairs {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) == 2 {
			labels[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return labels
}
