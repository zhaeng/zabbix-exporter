package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	ZabbixAPIRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "zabbix_exporter_api_requests_total",
			Help: "Zabbix JSON-RPC requests by method and bounded result status",
		},
		[]string{"method", "status"},
	)
	ZabbixAPIRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "zabbix_exporter_api_request_duration_seconds",
			Help:    "End-to-end duration of Zabbix JSON-RPC requests by method",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60},
		},
		[]string{"method"},
	)
	ZabbixAPIRequestsInFlight = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "zabbix_exporter_api_requests_in_flight",
			Help: "Zabbix JSON-RPC requests currently in flight by method",
		},
		[]string{"method"},
	)

	MetadataRefreshTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "zabbix_exporter_metadata_refresh_total",
			Help: "Total number of complete metadata refresh attempts by bounded status",
		},
		[]string{"status"},
	)
	MetadataRefreshDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "zabbix_exporter_metadata_refresh_duration_seconds",
			Help:    "Duration of complete metadata refresh attempts",
			Buckets: prometheus.DefBuckets,
		},
	)
	MetadataAge = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_metadata_age_seconds", Help: "Age of the current complete metadata snapshot"},
	)
	MetadataHosts = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_metadata_hosts", Help: "Hosts in the current complete metadata snapshot"},
	)
	MetadataItems = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_metadata_items", Help: "Items in the current complete metadata snapshot"},
	)
	MetadataEnabledItems = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_metadata_enabled_items", Help: "Enabled numeric items in the current complete metadata snapshot"},
	)

	CollectionGroups = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_collection_groups", Help: "Collection groups known to the scheduled pipeline"},
	)
	SchedulerQueueEntries = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_scheduler_queue_entries", Help: "Queued scheduled collection tasks"},
	)
	SchedulerInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_scheduler_inflight", Help: "Collection groups currently in flight"},
	)
	SchedulerLag = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "zabbix_exporter_scheduler_lag_seconds",
			Help:    "Delay between a planned group run and dispatch",
			Buckets: []float64{0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 300},
		},
	)
	GroupRunsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "zabbix_exporter_group_runs_total", Help: "Scheduled collection group outcomes"},
		[]string{"status", "delay_bucket"},
	)

	HistoryBatchesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "zabbix_exporter_history_batches_total", Help: "History batch requests by bounded result status"},
		[]string{"status"},
	)
	HistoryResponseRecords = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "zabbix_exporter_history_response_records",
			Help:    "Records returned by one history batch request",
			Buckets: prometheus.ExponentialBuckets(1, 2, 15),
		},
	)
	HistoryLimitHitsTotal = promauto.NewCounter(
		prometheus.CounterOpts{Name: "zabbix_exporter_history_limit_hits_total", Help: "History responses that reached their global limit"},
	)
	HistoryBatchDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "zabbix_exporter_history_batch_duration_seconds",
			Help:    "Duration of one history batch request",
			Buckets: prometheus.DefBuckets,
		},
	)

	ValueCacheEntries = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_value_cache_entries", Help: "Current ValueCache entries"},
	)
	ValueCacheValidEntries = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_value_cache_valid_entries", Help: "Valid ValueCache entries at the latest cache statistics observation"},
	)
	ValueCacheFreshEntries = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_value_cache_fresh_entries", Help: "Valid, unexpired ValueCache entries at the latest cache statistics observation"},
	)
	ValueCacheUpdatesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "zabbix_exporter_value_cache_updates_total", Help: "ValueCache apply outcomes"},
		[]string{"result"},
	)
	ValueCacheMissesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "zabbix_exporter_value_cache_misses_total", Help: "ValueCache miss outcomes"},
		[]string{"result"},
	)
	ValueCacheExpiredTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "zabbix_exporter_value_cache_expired_total", Help: "ValueCache deletions by bounded reason"},
		[]string{"reason"},
	)
	ExpiryBucketEntries = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_expiry_bucket_entries", Help: "Entries pending in expiry buckets"},
	)
	ExpiryPendingBuckets = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_expiry_pending_buckets", Help: "Non-empty expiry buckets"},
	)
	ExpiryLag = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_expiry_lag_seconds", Help: "Lag of the oldest overdue expiry bucket"},
	)
	ExpiryLimitedTotal = promauto.NewCounter(
		prometheus.CounterOpts{Name: "zabbix_exporter_expiry_limited_total", Help: "Expiry rounds stopped by the configured work bound"},
	)

	PushQueueEntries = promauto.NewGauge(
		prometheus.GaugeOpts{Name: "zabbix_exporter_push_queue_entries", Help: "Streaming Remote Write batches currently queued"},
	)
	PushBatchesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "zabbix_exporter_push_batches_total", Help: "Streaming Remote Write batch outcomes"},
		[]string{"status"},
	)
	PushBatchSeries = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "zabbix_exporter_push_batch_series",
			Help:    "Series in one streaming Remote Write batch",
			Buckets: prometheus.ExponentialBuckets(1, 2, 14),
		},
	)
	PushBatchBytes = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "zabbix_exporter_push_batch_bytes",
			Help:    "Streaming Remote Write batch bytes before and after compression",
			Buckets: prometheus.ExponentialBuckets(256, 2, 16),
		},
		[]string{"encoding"},
	)
	PushSlotDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "zabbix_exporter_push_slot_duration_seconds",
			Help:    "Duration of one streaming publish slot scan",
			Buckets: prometheus.DefBuckets,
		},
	)
	PushCoalescedCyclesTotal = promauto.NewCounter(
		prometheus.CounterOpts{Name: "zabbix_exporter_push_coalesced_cycles_total", Help: "Obsolete streaming cycle batches coalesced from the bounded queue"},
	)
)
