package metadata

import (
	"strconv"
	"strings"
	"time"
)

// ParseDelay parses only exact Zabbix simple intervals. Flexible, scheduled,
// and macro-based intervals are not representable by one duration, so they use
// the caller's conservative fallback and retain an explicit parse state.
func ParseDelay(raw string, fallback time.Duration) (time.Duration, DelayParseStatus) {
	if fallback <= 0 {
		fallback = time.Minute
	}

	value := strings.TrimSpace(raw)
	if value == "" {
		return fallback, DelayFallbackMissing
	}
	if strings.Contains(value, "{$") || strings.ContainsAny(value, "{}") {
		return fallback, DelayFallbackMacro
	}
	if strings.Contains(value, ";") {
		parts := strings.Split(value, ";")
		base := strings.TrimSpace(parts[0])
		if base == "" || base == "0" {
			return fallback, DelayFallbackScheduled
		}
		for _, interval := range parts[1:] {
			if strings.Contains(interval, "/") {
				return fallback, DelayFallbackFlexible
			}
		}
		return fallback, DelayFallbackScheduled
	}

	i := 0
	for i < len(value) && value[i] >= '0' && value[i] <= '9' {
		i++
	}
	if i == 0 {
		lower := strings.ToLower(value)
		if strings.HasPrefix(lower, "wd") || strings.HasPrefix(lower, "md") ||
			strings.HasPrefix(lower, "h") || strings.HasPrefix(lower, "m") || strings.HasPrefix(lower, "s") {
			return fallback, DelayFallbackScheduled
		}
		return fallback, DelayFallbackInvalid
	}
	n, err := strconv.ParseInt(value[:i], 10, 64)
	if err != nil || n <= 0 {
		return fallback, DelayFallbackInvalid
	}

	unit := strings.ToLower(strings.TrimSpace(value[i:]))
	var multiplier time.Duration
	switch unit {
	case "", "s":
		multiplier = time.Second
	case "m":
		multiplier = time.Minute
	case "h":
		multiplier = time.Hour
	case "d":
		multiplier = 24 * time.Hour
	case "w":
		multiplier = 7 * 24 * time.Hour
	default:
		return fallback, DelayFallbackInvalid
	}
	if n > int64((1<<63-1)/multiplier) {
		return fallback, DelayFallbackInvalid
	}
	return time.Duration(n) * multiplier, DelayParsed
}
