# Operations and Troubleshooting

English | [简体中文](operations_zh-CN.md)

## Health and readiness

- `GET /health` proves only that the HTTP process is alive.
- `GET /ready` requires a valid authentication token, a complete metadata snapshot, and a running scheduler. It returns HTTP 503 during shutdown.

Use `/health` as the Kubernetes liveness probe and `/ready` as the readiness probe. The initial metadata refresh can take time; increase the readiness failure threshold for large Zabbix installations.

## Recommended monitoring

### Metadata

```promql
zabbix_exporter_metadata_age_seconds
```

When monitoring this gauge, alert on:

- growth in `zabbix_exporter_metadata_refresh_total{result!="success"}`;
- metadata age remaining above the configured refresh interval;
- a sudden, significant drop in enabled item or host count.

### History

```promql
rate(zabbix_exporter_history_limit_hits_total[5m]) > 0
```

Also monitor history batch duration and status, scheduler lag, and queue entries. When limit hits occur together with increasing lag, confirm Zabbix API capacity before reducing batch size or adding controlled concurrency.

### Cache

```promql
zabbix_exporter_value_cache_fresh_entries
/
clamp_min(zabbix_exporter_metadata_enabled_items, 1)
```

`valid` means logically valid, while `fresh` also requires that the hard TTL derived from the source timestamp has not elapsed. After an item is disabled, its old state may remain until TTL, so the cache count can temporarily exceed the enabled item count.

### Remote Write

Monitor:

- `zabbix_exporter_push_queue_entries`
- `zabbix_exporter_push_batches_total{result=~"failed|queue_full|retryable|permanent"}`
- `zabbix_exporter_push_coalesced_cycles_total`
- batch series/bytes and slot duration

A queue that remains near capacity means Remote Write throughput is below the production rate. Check downstream throttling, networking, and authentication before adjusting workers or queue size. A larger queue only delays failure and increases memory usage.

## Common problems

### Authentication error during startup

1. In API-key mode, confirm `ZABBIX_API_KEY` is injected and does not expand to an empty string.
2. In username/password mode, confirm both fields are present.
3. Confirm the account can call the host, host-group, item, and history methods.
4. Check proxies, firewalls, the certificate chain, and the Zabbix API URL.

### `/ready` always returns 503

Inspect the response body and logs to determine whether the token is missing, metadata has not succeeded, or the scheduler is not running. Initial metadata loading must succeed completely; any failed item batch prevents the first complete snapshot.

### `/metrics` contains no business metrics

Check whether:

- host-group, host-IP, or item-key filters are too restrictive;
- items are enabled and use value type 0 or 3;
- history batches succeed;
- ValueCache fresh/valid entries are zero;
- item delay and source timestamps have caused samples to hard-expire.

### History limit is repeatedly reached

`history_max_limit` applies to the entire physical batch, not to each item. Reducing `history_batch_size` is normally safer than increasing the limit without bound. Address scheduler lag as well so the query window does not keep growing.

### Remote Write returns 400, 401, or 403

These are normally permanent errors and will not recover through retries. Check the URL, protocol compatibility, Basic Auth credentials, tenant-header requirements, and downstream logs. The current configuration supports Basic Auth directly but does not support custom headers.

### Remote Write returns 429/5xx or times out

These failures are retried with the configured backoff. Persistent failures occupy workers and increase queue depth. Restore downstream or network capacity before tuning `workers`, `max_retries`, or batch limits.

### Configuration changes do not take effect

The current version has no hot-reload endpoint. Perform a safe rolling restart of the process or Deployment.

## Capacity-tuning order

1. Record a baseline: host/item count, Zabbix API latency and errors, scheduler lag, cache coverage, CPU, and RSS.
2. Identify whether Zabbix or the Remote Write destination is the actual bottleneck.
3. Change one dimension at a time.
4. Before raising `history_query_concurrency`, monitor API errors and latency.
5. Before raising `workers`, confirm the Remote Write destination accepts more concurrency.
6. Do not hide sustained congestion behind an unbounded queue.

## Upgrade and rollback

- Use versioned images or release binaries; do not pin production permanently to `latest`.
- Save the previous image tag, configuration, and Secret references before deployment.
- Start with a limited rollout and verify `/ready`, series count, labels, timestamps, and Remote Write errors.
- A rollback loses the in-memory cache. The old version must rebuild metadata and ValueCache after startup.

## Collecting diagnostic information

Before opening an issue, provide:

- `zabbix-exporter --version`
- a redacted configuration
- Zabbix and downstream versions
- the affected time range and exporter logs
- relevant self-monitoring metrics

Remove API keys, passwords, internal domains/IPs, real hostnames, and unnecessary metric values.
