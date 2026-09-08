package filter

import (
	"regexp"

	"github.com/zhaeng/zabbix-exporter/internal/config"
)

// Filter 指标过滤器
type Filter struct {
	mode       string // "whitelist" 或 "blacklist"
	patterns   []*regexp.Regexp
	hostGroups []string
	valueTypes []string
}

// NewFilter 创建过滤器
func NewFilter(cfg config.MetricsFilterConfig) (*Filter, error) {
	f := &Filter{
		mode:       cfg.Filter.Mode,
		hostGroups: cfg.HostGroups,
		valueTypes: cfg.ValueTypes,
	}

	// 编译正则表达式
	for _, pattern := range cfg.Filter.Patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		f.patterns = append(f.patterns, re)
	}

	return f, nil
}

// ShouldKeep 检查指标是否应该保留
func (f *Filter) ShouldKeep(key string) bool {
	// 如果没有配置过滤模式，默认保留
	if len(f.patterns) == 0 {
		return true
	}

	matched := false
	for _, pattern := range f.patterns {
		if pattern.MatchString(key) {
			matched = true
			break
		}
	}

	switch f.mode {
	case "whitelist":
		// 白名单模式：只保留匹配的
		return matched
	case "blacklist":
		// 黑名单模式：排除匹配的
		return !matched
	default:
		return true
	}
}

// ShouldKeepHostGroup 检查主机组是否应该保留
func (f *Filter) ShouldKeepHostGroup(groupName string) bool {
	if len(f.hostGroups) == 0 {
		return true
	}

	for _, g := range f.hostGroups {
		if g == groupName {
			return true
		}
	}
	return false
}

// ShouldKeepValueType 检查值类型是否应该保留
func (f *Filter) ShouldKeepValueType(valueType string) bool {
	if len(f.valueTypes) == 0 {
		return true
	}

	// Zabbix value_type: 0=numeric float, 1=character, 2=log,
	// 3=numeric unsigned, 4=text, 5=binary.
	valueTypeMap := map[string]string{
		"0": "numeric",
		"1": "character",
		"2": "log",
		"3": "numeric",
		"4": "text",
		"5": "binary",
	}

	normalized := valueTypeMap[valueType]
	if normalized == "" {
		return false
	}

	for _, vt := range f.valueTypes {
		if vt == normalized || vt == valueType {
			return true
		}
	}
	return false
}
