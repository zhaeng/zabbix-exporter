# Configuration Reference

English | [简体中文](configuration_zh-CN.md)

The YAML configuration is processed with Go `os.ExpandEnv` before it is parsed. Both `${NAME}` and `$NAME` are expanded, and unset variables become empty strings. Start with [config.example.yaml](../config.example.yaml) and provide credentials through environment variables or a secrets manager.

## `zabbix`

| Field | Default | Description |
|---|---:|---|
| `url` | required | Zabbix JSON-RPC API URL. |
| `api_key` | empty | API key; takes precedence over username/password authentication. |
| `username` | empty | Required when no API key is configured. |
| `password` | empty | Required when no API key is configured. |
| `refresh_token_interval` | `300s` | Token refresh check interval for username/password authentication; must be greater than zero. |
| `tls_skip_verify` | `false` | Skip server certificate verification; use only in controlled environments. |

API-key mode does not call `user.login` or perform periodic token refreshes.

## `prometheus.pull`

| Field | Default | Description |
|---|---:|---|
| `enabled` | — | Compatibility field retained by the current version; the HTTP server always starts. |
| `port` | `9110` | HTTP listen port, in the range 1–65535. |
| `path` | `/metrics` | Path exposing business and self-monitoring metrics. |
| `internal_path` | `/internal/metrics` | Path exposing self-monitoring metrics only. |

Both metric paths must start with `/`, must be different, and must not use `/health` or `/ready`.

## `prometheus.push`

| Field | Default | Description |
|---|---:|---|
| `enabled` | `false` | Enable Prometheus Remote Write. |
| `interval` | `60s` | Full publishing cycle. |
| `spread_slots` | `12` | Number of slots used to spread one cycle; range 1–65535. |
| `page_size` | `5000` | Maximum cache entries read per page. |
| `max_batch_bytes` | `4194304` | Approximate maximum size of one uncompressed `WriteRequest`. |
| `workers` | `4` | Number of Remote Write workers. |
| `queue_capacity` | `100` | Capacity of the global bounded batch queue. |
| `max_retries` | `3` | Maximum retries for retryable failures. |
| `retry_backoff` | `100ms` | Initial retry backoff. |
| `max_retry_backoff` | `2s` | Maximum retry backoff. |

When push mode is enabled, `remote_write.endpoints` must contain exactly one entry:

| Field | Default | Description |
|---|---:|---|
| `url` | required | Remote Write URL. |
| `timeout` | `30s` | Timeout for one HTTP request. |
| `max_samples_per_send` | `2000` | Maximum samples in one batch. |
| `basic_auth.username` | empty | Optional Basic Auth username. |
| `basic_auth.password` | empty | Optional Basic Auth password. |

A full queue remains bounded instead of growing memory without limit; the rejected batch is reported through self-monitoring metrics. Network errors, HTTP 429, and HTTP 5xx are retryable. Other non-2xx responses are normally permanent failures.

## `collector`

| Field | Default | Description |
|---|---:|---|
| `workers` | `32` | Collection-group worker count. |
| `scheduler_queue_capacity` | `128` | Scheduler task queue capacity. |
| `history_query_concurrency` | `4` | Global maximum concurrent `history.get` calls. |
| `history_batch_size` | `50` | Number of items in one history batch. |
| `history_query_timeout` | `30s` | Timeout for one history request. |
| `history_max_limit` | `2000` | Maximum records returned by one `history.get`. |
| `query_overlap` | `5m` | Minimum overlap applied to incremental-query watermarks. |
| `startup_spread_window` | `60s` | Window used to spread the first scheduled runs. |
| `metadata_refresh_interval` | `20m` | Full metadata refresh interval. |
| `metadata_workers` | `2` | Concurrent `item.get` metadata requests. |
| `metadata_batch_hosts` | `50` | Hosts included in one metadata batch. |
| `metadata_request_timeout` | `30s` | Metadata request timeout. |
| `metadata_min_request_interval` | `100ms` | Minimum interval between metadata requests; may be zero. |
| `metadata_delay_fallback` | `60s` | Conservative interval used when an item delay cannot be parsed. |

A `history_max_limit` hit usually means a batch may be incomplete. Monitor `zabbix_exporter_history_limit_hits_total`; prefer reducing `history_batch_size` or collection backlog before increasing the limit.

## `labels`

- `global`: added to every host and item series.
- `host`: added to a host series and all item series for that host.
- `item`: added only to item series.

Label values support these complete templates:

- `{{host.id}}`
- `{{host.host}}` or `{{host.hostname}}` (technical host name)
- `{{host.name}}` (display name)
- `{{host.metadata.<field>}}`, `{{host.inventory.<field>}}`, or `{{inventory.<field>}}` (Zabbix host inventory fields)
- `{{host.ip}}`

The exporter generates required labels such as `__name__`, `host`, `host_ip`, and `vm_id`. Test custom labels that reuse built-in names before deploying them.

## `metrics`

| Field | Default | Description |
|---|---:|---|
| `filter.mode` | `whitelist` | Either `whitelist` or `blacklist`. |
| `filter.patterns` | empty | Go regular expressions matched against Zabbix item keys. An empty list retains all supported items. |
| `host_groups` | empty | Allowed host-group names; an empty list applies no restriction. |
| `host_ips` | empty | Allowed host IP addresses; an empty list applies no restriction. |
| `value_types` | empty | Accepts `numeric`, `0`, and `3`. At runtime, only Zabbix value types 0 and 3 are exported. |

Hosts with loopback, empty, or duplicate IP addresses are filtered during metadata ingestion.

## `cache`

| Field | Default | Description |
|---|---:|---|
| `shards` | `256` | ValueCache shard count; must be a power of two from 64 through 256. |
| `expiry_tick` | `1m` | Expiry-wheel processing interval. |
| `expiry_max_entries_per_run` | `100000` | Maximum expired entries processed in one run. |
| `miss_threshold` | `2` | Consecutive misses in fully successful batches required before an old value can be invalidated. |

The cache is memory-only and starts empty after a restart. Hard expiry is derived from the Zabbix source timestamp and item delay; scraping, pushing, or reading the cache does not extend the TTL.

## `log`

`level` accepts `debug`, `info`, `warn`, or `error`. Its default is `debug`; the public example uses `info`.
