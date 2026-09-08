# Self-monitoring Metrics

English | [简体中文](metrics_zh-CN.md)

`/internal/metrics` exposes only exporter, Go runtime, and process metrics. `/metrics` also includes Zabbix business series. The following tables list the main metrics defined by this project.

## Zabbix API

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `zabbix_exporter_api_requests_total` | Counter | `method,status` | JSON-RPC request results. |
| `zabbix_exporter_api_request_duration_seconds` | Histogram | `method` | End-to-end API request duration. |
| `zabbix_exporter_api_requests_in_flight` | Gauge | `method` | Currently active API requests. |

Average API duration:

```promql
sum by (method) (rate(zabbix_exporter_api_request_duration_seconds_sum[5m]))
/
sum by (method) (rate(zabbix_exporter_api_request_duration_seconds_count[5m]))
```

## Metadata

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `zabbix_exporter_metadata_refresh_total` | Counter | `status` | Full metadata refresh attempts. |
| `zabbix_exporter_metadata_refresh_duration_seconds` | Histogram | — | Full refresh duration. |
| `zabbix_exporter_metadata_age_seconds` | Gauge | — | Age of the current complete snapshot. |
| `zabbix_exporter_metadata_hosts` | Gauge | — | Hosts in the current snapshot. |
| `zabbix_exporter_metadata_items` | Gauge | — | Items in the current snapshot. |
| `zabbix_exporter_metadata_enabled_items` | Gauge | — | Enabled numeric items in the current snapshot. |

A failed refresh does not replace the last successful snapshot with partial data. Alert on refresh errors together with metadata age.

## Scheduler and history

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `zabbix_exporter_collection_groups` | Gauge | — | Current collection groups. |
| `zabbix_exporter_scheduler_queue_entries` | Gauge | — | Group tasks waiting to run. |
| `zabbix_exporter_scheduler_inflight` | Gauge | — | Groups currently running. |
| `zabbix_exporter_scheduler_lag_seconds` | Histogram | — | Delay from scheduled time to dispatch. |
| `zabbix_exporter_group_runs_total` | Counter | `status,delay_bucket` | Group-run results. |
| `zabbix_exporter_history_batches_total` | Counter | `status` | History-batch results. |
| `zabbix_exporter_history_response_records` | Histogram | — | Records returned per batch. |
| `zabbix_exporter_history_limit_hits_total` | Counter | — | Batches whose result count reached the global limit. |
| `zabbix_exporter_history_batch_duration_seconds` | Histogram | — | Duration of one history batch. |

Sustained growth in scheduler lag and queue depth indicates that collection capacity is below the task creation rate. A history limit hit means results may have been truncated.

## ValueCache and expiry

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `zabbix_exporter_value_cache_entries` | Gauge | — | Total cache entries. |
| `zabbix_exporter_value_cache_valid_entries` | Gauge | — | Logically valid cache entries. |
| `zabbix_exporter_value_cache_fresh_entries` | Gauge | — | Valid entries that have not reached hard TTL. |
| `zabbix_exporter_value_cache_updates_total` | Counter | `result` | Collection-result application outcomes. |
| `zabbix_exporter_value_cache_misses_total` | Counter | `result` | Miss-processing outcomes. |
| `zabbix_exporter_value_cache_expired_total` | Counter | `reason` | Entries removed by reason. |
| `zabbix_exporter_expiry_bucket_entries` | Gauge | — | Entries waiting in expiry buckets. |
| `zabbix_exporter_expiry_pending_buckets` | Gauge | — | Non-empty expiry buckets. |
| `zabbix_exporter_expiry_lag_seconds` | Gauge | — | Delay of the oldest overdue bucket. |
| `zabbix_exporter_expiry_limited_total` | Counter | — | Runs stopped by the per-run work limit. |

Cache coverage:

```promql
zabbix_exporter_value_cache_fresh_entries
/
clamp_min(zabbix_exporter_metadata_enabled_items, 1)
```

## Remote Write

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `zabbix_exporter_push_queue_entries` | Gauge | — | Batches currently queued. |
| `zabbix_exporter_push_batches_total` | Counter | `result` | Batch sending and queue outcomes. |
| `zabbix_exporter_push_batch_series` | Histogram | — | Series per batch. |
| `zabbix_exporter_push_batch_bytes` | Histogram | `encoding` | Uncompressed and compressed batch sizes. |
| `zabbix_exporter_push_slot_duration_seconds` | Histogram | — | Time spent building one publishing slot. |
| `zabbix_exporter_push_coalesced_cycles_total` | Counter | — | Old long-period batches replaced by a newer cycle. |

At minimum, alert when queue depth remains near capacity, permanent or retryable failures keep increasing, or coalesced cycles increase.

## Cardinality policy

Self-monitoring metrics do not use host, item ID, or metric name as labels, avoiding high cardinality in the exporter itself. Business-series cardinality is primarily determined by the number of Zabbix items, custom labels, and parameterized item keys.
