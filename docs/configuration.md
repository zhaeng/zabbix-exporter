# 配置参考 / Configuration Reference

配置文件使用 YAML，并在解析前通过 Go `os.ExpandEnv` 展开 `${NAME}` 或 `$NAME`。未设置的变量会展开为空字符串。推荐从 [config.example.yaml](../config.example.yaml) 复制配置，并通过 Secret 管理凭据。

The YAML file is expanded with Go `os.ExpandEnv` before parsing. Unset variables become empty strings. Start from [config.example.yaml](../config.example.yaml) and provide credentials through a secrets manager.

## zabbix

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `url` | 必填 | Zabbix JSON-RPC API 地址。 |
| `api_key` | 空 | API key；存在时优先于用户名和密码。 |
| `username` | 空 | 未配置 API key 时必填。 |
| `password` | 空 | 未配置 API key 时必填。 |
| `refresh_token_interval` | `300` 秒 | 用户名/密码认证的 token 刷新检查间隔，必须大于 0。 |
| `tls_skip_verify` | `false` | 跳过服务端证书验证；仅限受控环境。 |

API key 模式不会执行 `user.login` 或周期 token 刷新。

## prometheus.pull

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `enabled` | — | 当前版本保留的兼容字段；HTTP 服务始终启动。 |
| `port` | `9110` | HTTP 监听端口，范围 1–65535。 |
| `path` | `/metrics` | 业务指标及自监控指标路径。 |
| `internal_path` | `/internal/metrics` | 仅自监控指标路径。 |

两个指标路径必须以 `/` 开头、彼此不同，且不能占用 `/health` 或 `/ready`。

## prometheus.push

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `enabled` | `false` | 是否启用 Remote Write。 |
| `interval` | `60` 秒 | 完整发布周期。 |
| `spread_slots` | `12` | 将一个周期分成多少个错峰 slot，范围 1–65535。 |
| `page_size` | `5000` | 每次从缓存读取的最大条目数。 |
| `max_batch_bytes` | `4194304` | 单个未压缩 WriteRequest 的近似上限。 |
| `workers` | `4` | Remote Write worker 数。 |
| `queue_capacity` | `100` | 全局有界批次队列容量。 |
| `max_retries` | `3` | 可重试失败的最大重试次数。 |
| `retry_backoff` | `100ms` | 初始退避。 |
| `max_retry_backoff` | `2s` | 最大退避。 |

启用 Push 时，`remote_write.endpoints` 必须且只能有一个元素：

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `url` | 必填 | Remote Write URL。 |
| `timeout` | `30s` | 单次 HTTP 请求超时。 |
| `max_samples_per_send` | `2000` | 单批最大样本数。 |
| `basic_auth.username` | 空 | 可选 Basic Auth 用户名。 |
| `basic_auth.password` | 空 | 可选 Basic Auth 密码。 |

队列满时不会无限增长内存；对应批次失败并通过自监控指标暴露。HTTP 429、5xx 和网络错误属于可重试失败，其他非 2xx 通常按永久失败处理。

## collector

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `workers` | `32` | collection group worker 数。 |
| `scheduler_queue_capacity` | `128` | 调度任务队列容量。 |
| `history_query_concurrency` | `4` | 全局同时执行的 `history.get` 上限。 |
| `history_batch_size` | `50` | 单批 item 数。 |
| `history_query_timeout` | `30s` | 单次 history 请求超时。 |
| `history_max_limit` | `2000` | 单次 `history.get` 最大记录数。 |
| `query_overlap` | `5m` | 增量查询 watermark 的重叠窗口下限。 |
| `startup_spread_window` | `60s` | 首轮任务启动错峰窗口。 |
| `metadata_refresh_interval` | `20m` | metadata 完整刷新周期。 |
| `metadata_workers` | `2` | `item.get` metadata 并发数。 |
| `metadata_batch_hosts` | `50` | 单个 metadata 批次的 host 数。 |
| `metadata_request_timeout` | `30s` | metadata 请求超时。 |
| `metadata_min_request_interval` | `100ms` | metadata 请求之间的最小间隔，可为 0。 |
| `metadata_delay_fallback` | `60s` | 无法解析 item delay 时使用的保守周期。 |

`history_max_limit` 命中通常意味着一批数据可能不完整。应观察 `zabbix_exporter_history_limit_hits_total`，优先减小 `history_batch_size` 或缩短采集积压，而不是盲目放大 limit。

## labels

- `global`：添加到所有 host 和 item 序列。
- `host`：添加到 host 与该 host 的 item 序列。
- `item`：仅添加到 item 序列。

标签值支持以下完整模板：

- `{{host.id}}`
- `{{host.host}}` 或 `{{host.hostname}}`（技术主机名）
- `{{host.name}}`（显示名称）
- `{{host.metadata.<field>}}`、`{{host.inventory.<field>}}` 或 `{{inventory.<field>}}`（Zabbix host inventory 字段）
- `{{host.ip}}`

项目会生成必要的 `__name__`、`host`、`host_ip`、`vm_id` 等标签。自定义标签与内置标签重名时应先在测试环境确认结果。

## metrics

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `filter.mode` | `whitelist` | `whitelist` 或 `blacklist`。 |
| `filter.patterns` | 空 | 匹配 Zabbix item key 的 Go 正则。空数组保留全部支持项。 |
| `host_groups` | 空 | 允许的主机组名称；空数组不限制。 |
| `host_ips` | 空 | 允许的主机 IP；空数组不限制。 |
| `value_types` | 空 | 支持 `numeric`、`0` 和 `3`。运行时只导出 Zabbix value type 0/3。 |

loopback、空 IP 和重复 IP 的主机会在 metadata 入口过滤。

## cache

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `shards` | `256` | ValueCache 分片数；必须是 64–256 之间的 2 的幂。 |
| `expiry_tick` | `1m` | 到期轮运行周期。 |
| `expiry_max_entries_per_run` | `100000` | 单轮最多处理的到期项。 |
| `miss_threshold` | `2` | 完整成功批次连续 miss 达到该值后允许失效旧值。 |

缓存仅在内存中，重启后从空状态重新采集。硬过期时间从 Zabbix 源时间和 item delay 计算，抓取、推送或再次读缓存不会延长 TTL。

## log

`level` 支持 `debug`、`info`、`warn` 和 `error`，默认 `debug`；公开示例使用 `info`。
