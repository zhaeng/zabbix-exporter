package filter

import (
	"testing"

	"github.com/zhaeng/zabbix-exporter/internal/config"
)

func TestFilterModes(t *testing.T) {
	tests := []struct {
		name string
		mode string
		key  string
		want bool
	}{
		{name: "whitelist match", mode: "whitelist", key: "system.cpu.load", want: true},
		{name: "whitelist miss", mode: "whitelist", key: "vm.memory.size", want: false},
		{name: "blacklist match", mode: "blacklist", key: "system.cpu.load", want: false},
		{name: "blacklist miss", mode: "blacklist", key: "vm.memory.size", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter, err := NewFilter(config.MetricsFilterConfig{
				Filter: config.FilterConfig{Mode: tt.mode, Patterns: []string{`^system\.cpu\.`}},
			})
			if err != nil {
				t.Fatalf("NewFilter() error = %v", err)
			}
			if got := filter.ShouldKeep(tt.key); got != tt.want {
				t.Fatalf("ShouldKeep(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestFilterRejectsInvalidPattern(t *testing.T) {
	_, err := NewFilter(config.MetricsFilterConfig{
		Filter: config.FilterConfig{Mode: "whitelist", Patterns: []string{"["}},
	})
	if err == nil {
		t.Fatal("NewFilter() error = nil, want invalid regexp error")
	}
}
