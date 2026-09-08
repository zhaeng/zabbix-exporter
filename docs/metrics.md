# 自监控指标 / Self-monitoring Metrics

`/internal/metrics` 只暴露 exporter、Go runtime 和 process 指标；`/metrics` 还包含 Zabbix 业务序列。以下是项目自身定义的主要指标。

## Zabbix API

| 指标 | 类型 | 标签 | 含义 |
|---|---|---|---|
| `zabbix_exporter_api_requests_total` | Counter | `method,status` | JSON-RPC 请求结果数。 |
| `zabbix_exporter_api_request_duration_seconds` | Histogram | `method` | 端到端 API 请求耗时。 |
| `zabbix_exporter_api_requests_in_flight` | Gauge | `method` | 当前 API 并发请求数。 |

平均 API 耗时：

```promql
sum by (method) (rate(zabbix_exporter_api_request_duration_seconds_sum[5m]))
/
sum by (method) (rate(zabbix_exporter_api_request_duration_seconds_count[5m]))
```

## Metadata

| 指标 | 类型 | 标签 | 含义 |
|---|---|---|---|
| `zabbix_exporter_metadata_refresh_total` | Counter | `status` | 完整刷新尝试数。 |
| `zabbix_exporter_metadata_refresh_duration_seconds` | Histogram | — | 完整刷新耗时。 |
| `zabbix_exporter_metadata_age_seconds` | Gauge | — | 当前完整快照年龄。 |
| `zabbix_exporter_metadata_hosts` | Gauge | — | 当前快照 host 数。 |
| `zabbix_exporter_metadata_items` | Gauge | — | 当前快照 item 数。 |
| `zabbix_exporter_metadata_enabled_items` | Gauge | — | 当前启用的数值 item 数。 |

刷新失败不会用不完整结果替换最后一个成功快照，因此 refresh error 与 metadata age 应组合告警。

## Scheduler 与 History

| 指标 | 类型 | 标签 | 含义 |
|---|---|---|---|
| `zabbix_exporter_collection_groups` | Gauge | — | 当前 collection group 数。 |
| `zabbix_exporter_scheduler_queue_entries` | Gauge | — | 等待执行的 group task 数。 |
| `zabbix_exporter_scheduler_inflight` | Gauge | — | 正在执行的 group 数。 |
| `zabbix_exporter_scheduler_lag_seconds` | Histogram | — | 计划时间到派发时间的延迟。 |
| `zabbix_exporter_group_runs_total` | Counter | `status,delay_bucket` | group 执行结果。 |
| `zabbix_exporter_history_batches_total` | Counter | `status` | history batch 结果。 |
| `zabbix_exporter_history_response_records` | Histogram | — | 单批返回记录数。 |
| `zabbix_exporter_history_limit_hits_total` | Counter | — | 返回数达到全局 limit 的批次数。 |
| `zabbix_exporter_history_batch_duration_seconds` | Histogram | — | 单个 history batch 耗时。 |

Scheduler lag 和 queue 持续上升表示采集能力低于任务产生速度。History limit hit 表示结果可能被截断。

## ValueCache 与 Expiry

| 指标 | 类型 | 标签 | 含义 |
|---|---|---|---|
| `zabbix_exporter_value_cache_entries` | Gauge | — | 缓存总条目数。 |
| `zabbix_exporter_value_cache_valid_entries` | Gauge | — | 当前逻辑有效条目数。 |
| `zabbix_exporter_value_cache_fresh_entries` | Gauge | — | 有效且未到硬 TTL 的条目数。 |
| `zabbix_exporter_value_cache_updates_total` | Counter | `result` | 采集结果应用状态。 |
| `zabbix_exporter_value_cache_misses_total` | Counter | `result` | miss 处理状态。 |
| `zabbix_exporter_value_cache_expired_total` | Counter | `reason` | 删除原因统计。 |
| `zabbix_exporter_expiry_bucket_entries` | Gauge | — | expiry bucket 中等待处理的条目。 |
| `zabbix_exporter_expiry_pending_buckets` | Gauge | — | 非空 expiry bucket 数。 |
| `zabbix_exporter_expiry_lag_seconds` | Gauge | — | 最老逾期 bucket 的延迟。 |
| `zabbix_exporter_expiry_limited_total` | Counter | — | 单轮因工作上限停止的次数。 |

缓存覆盖率：

```promql
zabbix_exporter_value_cache_fresh_entries
/
clamp_min(zabbix_exporter_metadata_enabled_items, 1)
```

## Remote Write

| 指标 | 类型 | 标签 | 含义 |
|---|---|---|---|
| `zabbix_exporter_push_queue_entries` | Gauge | — | 当前排队批次数。 |
| `zabbix_exporter_push_batches_total` | Counter | `result` | 批次发送和队列结果。 |
| `zabbix_exporter_push_batch_series` | Histogram | — | 单批序列数。 |
| `zabbix_exporter_push_batch_bytes` | Histogram | `encoding` | 未压缩和压缩后的批次大小。 |
| `zabbix_exporter_push_slot_duration_seconds` | Histogram | — | 构建一个发布 slot 的耗时。 |
| `zabbix_exporter_push_coalesced_cycles_total` | Counter | — | 被新周期替代的旧长周期批次数。 |

建议至少对 queue 长期接近容量、永久失败、retryable 失败持续增长和 coalesced cycle 增长设置告警。

## 基数原则

自监控指标不使用 host、item ID 或 metric name 作为标签，避免 exporter 自身产生高基数。业务指标的基数主要由 Zabbix item 数、自定义 labels 和参数化 item key 决定。
