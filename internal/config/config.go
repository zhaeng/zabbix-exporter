package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 应用配置
type Config struct {
	Zabbix     ZabbixConfig        `yaml:"zabbix"`
	Prometheus PrometheusConfig    `yaml:"prometheus"`
	Labels     LabelsConfig        `yaml:"labels"`
	Metrics    MetricsFilterConfig `yaml:"metrics"`
	Collector  CollectorConfig     `yaml:"collector"`
	Cache      CacheConfig         `yaml:"cache"`
	Log        LogConfig           `yaml:"log"`
}

// LogConfig 日志配置
type LogConfig struct {
	Level string `yaml:"level"` // debug, info, warn, error
}

// ZabbixConfig Zabbix 连接配置
type ZabbixConfig struct {
	URL                  string `yaml:"url"`
	Username             string `yaml:"username"`
	Password             string `yaml:"password"`
	APIKey               string `yaml:"api_key"`
	TLSSkipVerify        bool   `yaml:"tls_skip_verify"`
	RefreshTokenInterval int    `yaml:"refresh_token_interval"` // 提前刷新 token 秒数
}

// PrometheusConfig Prometheus 配置
type PrometheusConfig struct {
	Pull PullConfig `yaml:"pull"`
	Push PushConfig `yaml:"push"`
}

// PullConfig Pull 模式配置
type PullConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Port         int    `yaml:"port"`
	Path         string `yaml:"path"`
	InternalPath string `yaml:"internal_path"`
}

// PushConfig Push 模式配置
type PushConfig struct {
	Enabled         bool              `yaml:"enabled"`
	RemoteWrite     RemoteWriteConfig `yaml:"remote_write"`
	Interval        int               `yaml:"interval"` // 推送间隔（秒）
	SpreadSlots     int               `yaml:"spread_slots"`
	PageSize        int               `yaml:"page_size"`
	MaxBatchBytes   int               `yaml:"max_batch_bytes"`
	Workers         int               `yaml:"workers"`
	QueueCapacity   int               `yaml:"queue_capacity"`
	MaxRetries      int               `yaml:"max_retries"`
	RetryBackoff    time.Duration     `yaml:"retry_backoff"`
	MaxRetryBackoff time.Duration     `yaml:"max_retry_backoff"`
}

// RemoteWriteConfig Remote Write 配置
type RemoteWriteConfig struct {
	Endpoints []RemoteWriteEndpoint `yaml:"endpoints"`
}

// RemoteWriteEndpoint 单个 Remote Write 端点配置
type RemoteWriteEndpoint struct {
	URL               string          `yaml:"url"`
	Timeout           time.Duration   `yaml:"timeout"`
	MaxSamplesPerSend int             `yaml:"max_samples_per_send"`
	BasicAuth         BasicAuthConfig `yaml:"basic_auth"`
}

// BasicAuthConfig Basic Auth 认证配置
type BasicAuthConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// LabelsConfig Label 配置
type LabelsConfig struct {
	Global map[string]string `yaml:"global"`
	Host   map[string]string `yaml:"host"`
	Item   map[string]string `yaml:"item"`
}

// MetricsFilterConfig 指标过滤配置
type MetricsFilterConfig struct {
	Filter     FilterConfig `yaml:"filter"`
	HostGroups []string     `yaml:"host_groups"`
	HostIPs    []string     `yaml:"host_ips"` // 只采集指定 IP 的主机指标
	ValueTypes []string     `yaml:"value_types"`
}

// FilterConfig 过滤模式配置
type FilterConfig struct {
	Mode     string   `yaml:"mode"` // "whitelist" 或 "blacklist"
	Patterns []string `yaml:"patterns"`
}

// CollectorConfig 采集并发配置
type CollectorConfig struct {
	Workers                    int           `yaml:"workers"`                       // scheduled worker 数
	SchedulerQueueCapacity     int           `yaml:"scheduler_queue_capacity"`      // scheduled 有界任务队列
	HistoryBatchSize           int           `yaml:"history_batch_size"`            // history.get 每批 item 数
	HistoryQueryTimeout        time.Duration `yaml:"history_query_timeout"`         // scheduled 单次 history 请求超时
	HistoryMaxLimit            int           `yaml:"history_max_limit"`             // 单次 history.get 最大记录数
	QueryOverlap               time.Duration `yaml:"query_overlap"`                 // history watermark 重叠窗口下限
	StartupSpreadWindow        time.Duration `yaml:"startup_spread_window"`         // scheduled 启动错峰窗口
	HistoryQueryConcurrency    int           `yaml:"history_query_concurrency"`     // 全局 history 并发上限
	MetadataRefreshInterval    time.Duration `yaml:"metadata_refresh_interval"`     // metadata 低频刷新周期
	MetadataWorkers            int           `yaml:"metadata_workers"`              // metadata 独立并发上限
	MetadataBatchHosts         int           `yaml:"metadata_batch_hosts"`          // 每个 item.get 批次的主机数
	MetadataRequestTimeout     time.Duration `yaml:"metadata_request_timeout"`      // 单次 host/item metadata 请求超时
	MetadataMinRequestInterval time.Duration `yaml:"metadata_min_request_interval"` // metadata 请求之间的最小间隔
	MetadataDelayFallback      time.Duration `yaml:"metadata_delay_fallback"`       // 无法解析 delay 时的保守周期
}

// CacheConfig 缓存配置
type CacheConfig struct {
	Shards                 int           `yaml:"shards"`                     // scheduled ValueCache 分片数
	ExpiryTick             time.Duration `yaml:"expiry_tick"`                // scheduled 到期处理周期
	ExpiryMaxEntriesPerRun int           `yaml:"expiry_max_entries_per_run"` // 单轮到期处理上限
	MissThreshold          int           `yaml:"miss_threshold"`             // 完整成功批次连续 miss 阈值
}

// Load 加载配置文件
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// 支持环境变量替换
	data = []byte(os.ExpandEnv(string(data)))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// 设置默认值
	cfg.setDefaults()

	// 验证配置
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// setDefaults 设置默认值
func (c *Config) setDefaults() {
	if c.Zabbix.RefreshTokenInterval == 0 {
		c.Zabbix.RefreshTokenInterval = 300
	}

	if c.Prometheus.Pull.Port == 0 {
		c.Prometheus.Pull.Port = 9110
	}
	if c.Prometheus.Pull.Path == "" {
		c.Prometheus.Pull.Path = "/metrics"
	}
	if c.Prometheus.Pull.InternalPath == "" {
		c.Prometheus.Pull.InternalPath = "/internal/metrics"
	}

	if c.Prometheus.Push.Interval == 0 {
		c.Prometheus.Push.Interval = 60
	}
	if c.Prometheus.Push.SpreadSlots == 0 {
		c.Prometheus.Push.SpreadSlots = 12
	}
	if c.Prometheus.Push.PageSize == 0 {
		c.Prometheus.Push.PageSize = 5000
	}
	if c.Prometheus.Push.MaxBatchBytes == 0 {
		c.Prometheus.Push.MaxBatchBytes = 4 << 20
	}
	if c.Prometheus.Push.Workers == 0 {
		c.Prometheus.Push.Workers = 4
	}
	if c.Prometheus.Push.QueueCapacity == 0 {
		c.Prometheus.Push.QueueCapacity = 100
	}
	if c.Prometheus.Push.MaxRetries == 0 {
		c.Prometheus.Push.MaxRetries = 3
	}
	if c.Prometheus.Push.RetryBackoff == 0 {
		c.Prometheus.Push.RetryBackoff = 100 * time.Millisecond
	}
	if c.Prometheus.Push.MaxRetryBackoff == 0 {
		c.Prometheus.Push.MaxRetryBackoff = 2 * time.Second
	}
	for i := range c.Prometheus.Push.RemoteWrite.Endpoints {
		if c.Prometheus.Push.RemoteWrite.Endpoints[i].Timeout == 0 {
			c.Prometheus.Push.RemoteWrite.Endpoints[i].Timeout = 30 * time.Second
		}
		if c.Prometheus.Push.RemoteWrite.Endpoints[i].MaxSamplesPerSend == 0 {
			c.Prometheus.Push.RemoteWrite.Endpoints[i].MaxSamplesPerSend = 2000
		}
	}

	if c.Metrics.Filter.Mode == "" {
		c.Metrics.Filter.Mode = "whitelist"
	}

	if c.Collector.Workers == 0 {
		c.Collector.Workers = 32
	}
	if c.Collector.SchedulerQueueCapacity == 0 {
		c.Collector.SchedulerQueueCapacity = 128
	}
	if c.Collector.HistoryBatchSize == 0 {
		c.Collector.HistoryBatchSize = 50
	}
	if c.Collector.HistoryQueryTimeout == 0 {
		c.Collector.HistoryQueryTimeout = 30 * time.Second
	}
	if c.Collector.HistoryMaxLimit == 0 {
		c.Collector.HistoryMaxLimit = 2000
	}
	if c.Collector.QueryOverlap == 0 {
		c.Collector.QueryOverlap = 5 * time.Minute
	}
	if c.Collector.StartupSpreadWindow == 0 {
		c.Collector.StartupSpreadWindow = time.Minute
	}
	if c.Collector.HistoryQueryConcurrency == 0 {
		c.Collector.HistoryQueryConcurrency = 4 // 默认 4
	}
	if c.Collector.MetadataRefreshInterval == 0 {
		c.Collector.MetadataRefreshInterval = 20 * time.Minute
	}
	if c.Collector.MetadataWorkers == 0 {
		c.Collector.MetadataWorkers = 2
	}
	if c.Collector.MetadataBatchHosts == 0 {
		c.Collector.MetadataBatchHosts = 50
	}
	if c.Collector.MetadataRequestTimeout == 0 {
		c.Collector.MetadataRequestTimeout = 30 * time.Second
	}
	if c.Collector.MetadataMinRequestInterval == 0 {
		c.Collector.MetadataMinRequestInterval = 100 * time.Millisecond
	}
	if c.Collector.MetadataDelayFallback == 0 {
		c.Collector.MetadataDelayFallback = time.Minute
	}

	if c.Cache.Shards == 0 {
		c.Cache.Shards = 256
	}
	if c.Cache.ExpiryTick == 0 {
		c.Cache.ExpiryTick = time.Minute
	}
	if c.Cache.ExpiryMaxEntriesPerRun == 0 {
		c.Cache.ExpiryMaxEntriesPerRun = 100_000
	}
	if c.Cache.MissThreshold == 0 {
		c.Cache.MissThreshold = 2
	}

	if c.Log.Level == "" {
		c.Log.Level = "debug" // 默认 debug 级别
	}
}

// validate 验证配置
func (c *Config) validate() error {
	if c.Zabbix.URL == "" {
		return fmt.Errorf("zabbix.url is required")
	}
	if c.Zabbix.APIKey == "" {
		if c.Zabbix.Username == "" {
			return fmt.Errorf("zabbix.username is required when zabbix.api_key is not configured")
		}
		if c.Zabbix.Password == "" {
			return fmt.Errorf("zabbix.password is required when zabbix.api_key is not configured")
		}
	}
	if c.Zabbix.RefreshTokenInterval < 1 {
		return fmt.Errorf("zabbix.refresh_token_interval must be positive")
	}

	if c.Prometheus.Pull.Port < 1 || c.Prometheus.Pull.Port > 65535 {
		return fmt.Errorf("prometheus.pull.port must be between 1 and 65535")
	}
	if c.Prometheus.Pull.Path[0] != '/' {
		return fmt.Errorf("prometheus.pull.path must start with '/'")
	}
	if c.Prometheus.Pull.InternalPath[0] != '/' {
		return fmt.Errorf("prometheus.pull.internal_path must start with '/'")
	}
	if c.Prometheus.Pull.InternalPath == c.Prometheus.Pull.Path {
		return fmt.Errorf("prometheus.pull.internal_path must differ from prometheus.pull.path")
	}
	if c.Prometheus.Pull.Path == "/health" || c.Prometheus.Pull.Path == "/ready" {
		return fmt.Errorf("prometheus.pull.path conflicts with a reserved endpoint")
	}
	if c.Prometheus.Pull.InternalPath == "/health" || c.Prometheus.Pull.InternalPath == "/ready" {
		return fmt.Errorf("prometheus.pull.internal_path conflicts with a reserved endpoint")
	}

	if c.Prometheus.Push.Enabled {
		if len(c.Prometheus.Push.RemoteWrite.Endpoints) == 0 {
			return fmt.Errorf("prometheus.push.remote_write.endpoints is required when push is enabled")
		}
		for _, ep := range c.Prometheus.Push.RemoteWrite.Endpoints {
			if ep.URL == "" {
				return fmt.Errorf("prometheus.push.remote_write.endpoints[].url is required")
			}
			if ep.Timeout <= 0 {
				return fmt.Errorf("prometheus.push.remote_write.endpoints[].timeout must be positive")
			}
			if ep.MaxSamplesPerSend < 1 {
				return fmt.Errorf("prometheus.push.remote_write.endpoints[].max_samples_per_send must be positive")
			}
		}
	}
	if c.Prometheus.Push.Enabled && len(c.Prometheus.Push.RemoteWrite.Endpoints) != 1 {
		return fmt.Errorf("prometheus.push requires exactly one remote write endpoint")
	}
	if c.Prometheus.Push.Interval < 1 {
		return fmt.Errorf("prometheus.push.interval must be positive")
	}
	if c.Prometheus.Push.SpreadSlots < 1 || c.Prometheus.Push.SpreadSlots > 65535 {
		return fmt.Errorf("prometheus.push.spread_slots must be between 1 and 65535")
	}
	if c.Prometheus.Push.PageSize < 1 {
		return fmt.Errorf("prometheus.push.page_size must be positive")
	}
	if c.Prometheus.Push.MaxBatchBytes < 1 {
		return fmt.Errorf("prometheus.push.max_batch_bytes must be positive")
	}
	if c.Prometheus.Push.Workers < 1 {
		return fmt.Errorf("prometheus.push.workers must be positive")
	}
	if c.Prometheus.Push.QueueCapacity < 1 {
		return fmt.Errorf("prometheus.push.queue_capacity must be positive")
	}
	if c.Prometheus.Push.MaxRetries < 0 {
		return fmt.Errorf("prometheus.push.max_retries cannot be negative")
	}
	if c.Prometheus.Push.RetryBackoff < 0 || c.Prometheus.Push.MaxRetryBackoff <= 0 || c.Prometheus.Push.RetryBackoff > c.Prometheus.Push.MaxRetryBackoff {
		return fmt.Errorf("invalid prometheus.push retry backoff range")
	}

	if c.Metrics.Filter.Mode != "whitelist" && c.Metrics.Filter.Mode != "blacklist" {
		return fmt.Errorf("metrics.filter.mode must be 'whitelist' or 'blacklist'")
	}
	if c.Collector.MetadataRefreshInterval <= 0 {
		return fmt.Errorf("collector.metadata_refresh_interval must be positive")
	}
	if c.Collector.MetadataWorkers < 1 {
		return fmt.Errorf("collector.metadata_workers must be at least 1")
	}
	if c.Collector.MetadataBatchHosts < 1 {
		return fmt.Errorf("collector.metadata_batch_hosts must be at least 1")
	}
	if c.Collector.MetadataRequestTimeout <= 0 {
		return fmt.Errorf("collector.metadata_request_timeout must be positive")
	}
	if c.Collector.MetadataMinRequestInterval < 0 {
		return fmt.Errorf("collector.metadata_min_request_interval cannot be negative")
	}
	if c.Collector.MetadataDelayFallback <= 0 {
		return fmt.Errorf("collector.metadata_delay_fallback must be positive")
	}
	if c.Collector.HistoryQueryConcurrency < 1 {
		return fmt.Errorf("collector.history_query_concurrency must be positive")
	}
	if c.Collector.Workers < 1 {
		return fmt.Errorf("collector.workers must be positive")
	}
	if c.Collector.SchedulerQueueCapacity < 1 {
		return fmt.Errorf("collector.scheduler_queue_capacity must be positive")
	}
	if c.Collector.HistoryBatchSize < 1 {
		return fmt.Errorf("collector.history_batch_size must be positive")
	}
	if c.Collector.HistoryQueryTimeout <= 0 {
		return fmt.Errorf("collector.history_query_timeout must be positive")
	}
	if c.Collector.HistoryMaxLimit < 1 {
		return fmt.Errorf("collector.history_max_limit must be positive")
	}
	if c.Collector.QueryOverlap < 0 {
		return fmt.Errorf("collector.query_overlap cannot be negative")
	}
	if c.Collector.StartupSpreadWindow < 0 {
		return fmt.Errorf("collector.startup_spread_window cannot be negative")
	}
	if c.Cache.Shards < 64 || c.Cache.Shards > 256 || c.Cache.Shards&(c.Cache.Shards-1) != 0 {
		return fmt.Errorf("cache.shards must be a power of two between 64 and 256")
	}
	if c.Cache.ExpiryTick <= 0 {
		return fmt.Errorf("cache.expiry_tick must be positive")
	}
	if c.Cache.ExpiryMaxEntriesPerRun < 1 {
		return fmt.Errorf("cache.expiry_max_entries_per_run must be positive")
	}
	if c.Cache.MissThreshold < 1 {
		return fmt.Errorf("cache.miss_threshold must be positive")
	}
	return nil
}
