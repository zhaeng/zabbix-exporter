package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/zhaeng/zabbix-exporter/internal/cache"
	"github.com/zhaeng/zabbix-exporter/internal/collector"
	"github.com/zhaeng/zabbix-exporter/internal/config"
	"github.com/zhaeng/zabbix-exporter/internal/filter"
	"github.com/zhaeng/zabbix-exporter/internal/metadata"
	"github.com/zhaeng/zabbix-exporter/internal/promwrap"
	"github.com/zhaeng/zabbix-exporter/internal/scheduler"
	"github.com/zhaeng/zabbix-exporter/internal/zabbix"
)

// NewScheduledFromApp maps the application config to the only supported
// metadata→scheduler→expiry→streaming publisher runtime.
func NewScheduledFromApp(client *zabbix.Client, app *config.Config) (*Scheduled, error) {
	if client == nil || app == nil {
		return nil, fmt.Errorf("zabbix client and application config are required")
	}
	itemFilter, err := filter.NewFilter(app.Metrics)
	if err != nil {
		return nil, fmt.Errorf("build scheduled metric filter: %w", err)
	}
	reader := &filteredAppReader{client: client, filter: itemFilter, config: app.Metrics}
	scheduledConfig, err := ScheduledConfigFromApp(app)
	if err != nil {
		return nil, err
	}
	return NewScheduled(scheduledConfig, reader, client)
}

func ScheduledConfigFromApp(app *config.Config) (ScheduledConfig, error) {
	if app == nil {
		return ScheduledConfig{}, fmt.Errorf("application config is required")
	}
	overlapMax := collector.DefaultOverlapMax
	if app.Collector.QueryOverlap > overlapMax {
		overlapMax = app.Collector.QueryOverlap
	}
	cfg := ScheduledConfig{
		Metadata: metadata.RefresherConfigFromApp(app),
		Scheduler: scheduler.Config{
			BatchSize:       app.Collector.HistoryBatchSize,
			WorkerCount:     app.Collector.Workers,
			QueueCapacity:   app.Collector.SchedulerQueueCapacity,
			BootstrapSpread: app.Collector.StartupSpreadWindow,
		},
		History: collector.HistoryConfig{
			Limit:        app.Collector.HistoryMaxLimit,
			OverlapMin:   app.Collector.QueryOverlap,
			OverlapMax:   overlapMax,
			QueryTimeout: app.Collector.HistoryQueryTimeout,
		},
		HistoryConcurrency: app.Collector.HistoryQueryConcurrency,
		ValueCache: cache.ValueCacheConfig{
			ShardCount:    app.Cache.Shards,
			PublishSlots:  app.Prometheus.Push.SpreadSlots,
			MissThreshold: uint32(app.Cache.MissThreshold),
		},
		Expiry: cache.ExpiryWheelConfig{
			MaxEntriesPerRun: app.Cache.ExpiryMaxEntriesPerRun,
		},
		ExpiryTick:      app.Cache.ExpiryTick,
		PublishInterval: time.Duration(app.Prometheus.Push.Interval) * time.Second,
		ShutdownTimeout: defaultShutdownTimeout,
	}
	if app.Prometheus.Push.Enabled {
		if len(app.Prometheus.Push.RemoteWrite.Endpoints) != 1 {
			return ScheduledConfig{}, fmt.Errorf("scheduled push requires exactly one remote write endpoint")
		}
		endpoint := app.Prometheus.Push.RemoteWrite.Endpoints[0]
		cfg.Publisher = &promwrap.StreamingPublisherConfig{
			EndpointURL:     endpoint.URL,
			Timeout:         endpoint.Timeout,
			Username:        endpoint.BasicAuth.Username,
			Password:        endpoint.BasicAuth.Password,
			SpreadSlots:     uint16(app.Prometheus.Push.SpreadSlots),
			PageSize:        app.Prometheus.Push.PageSize,
			MaxSamples:      endpoint.MaxSamplesPerSend,
			MaxBatchBytes:   app.Prometheus.Push.MaxBatchBytes,
			Workers:         app.Prometheus.Push.Workers,
			QueueCapacity:   app.Prometheus.Push.QueueCapacity,
			MaxRetries:      app.Prometheus.Push.MaxRetries,
			RetryBackoff:    app.Prometheus.Push.RetryBackoff,
			MaxRetryBackoff: app.Prometheus.Push.MaxRetryBackoff,
		}
	}
	return cfg, nil
}

// filteredAppReader applies the configured host group, host IP, duplicate-IP,
// item-key and numeric value-type filters. It uses item.get only on the
// low-frequency metadata path and never requests values.
type filteredAppReader struct {
	client *zabbix.Client
	filter *filter.Filter
	config config.MetricsFilterConfig
}

func (r *filteredAppReader) GetHosts(ctx context.Context) ([]zabbix.Host, error) {
	var hosts []zabbix.Host
	var err error
	if len(r.config.HostGroups) == 0 {
		hosts, err = r.client.GetHosts(ctx)
	} else {
		groups, groupErr := r.client.GetHostGroups(ctx)
		if groupErr != nil {
			return nil, groupErr
		}
		groupIDs := make([]string, 0, len(groups))
		for _, group := range groups {
			if r.filter.ShouldKeepHostGroup(group.Name) {
				groupIDs = append(groupIDs, group.GroupID)
			}
		}
		hosts, err = r.client.GetHostsByGroupIDs(ctx, groupIDs)
	}
	if err != nil {
		return nil, err
	}
	allowedIPs := make(map[string]struct{}, len(r.config.HostIPs))
	for _, ip := range r.config.HostIPs {
		allowedIPs[ip] = struct{}{}
	}
	seenIPs := make(map[string]struct{}, len(hosts))
	filtered := make([]zabbix.Host, 0, len(hosts))
	for _, host := range hosts {
		ip := selectedHostIP(host)
		if ip == "" || ip == "127.0.0.1" {
			continue
		}
		if len(allowedIPs) != 0 {
			if _, ok := allowedIPs[ip]; !ok {
				continue
			}
		}
		if _, duplicate := seenIPs[ip]; duplicate {
			continue
		}
		seenIPs[ip] = struct{}{}
		filtered = append(filtered, host)
	}
	return filtered, nil
}

func (r *filteredAppReader) GetItemMetadata(ctx context.Context, hostIDs []string) ([]zabbix.Item, error) {
	items, err := r.client.GetItemMetadata(ctx, hostIDs)
	if err != nil {
		return nil, err
	}
	filtered := make([]zabbix.Item, 0, len(items))
	for _, item := range items {
		// Only numeric float/unsigned values are exportable. The application
		// filter may further narrow that supported set (for example 0 or 3).
		if isExportableValueType(item.ValueType) &&
			r.filter.ShouldKeepValueType(item.ValueType) && r.filter.ShouldKeep(item.Key) {
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
}

func selectedHostIP(host zabbix.Host) string {
	for _, iface := range host.Interfaces {
		if (iface.Type == "2" || iface.Main == "1") && iface.IP != "" {
			return iface.IP
		}
	}
	for _, iface := range host.Interfaces {
		if iface.IP != "" {
			return iface.IP
		}
	}
	return ""
}

func isExportableValueType(valueType string) bool {
	return valueType == "0" || valueType == "3"
}

var _ metadata.Reader = (*filteredAppReader)(nil)
