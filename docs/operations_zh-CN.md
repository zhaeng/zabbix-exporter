# 运维与排障

[English](operations.md) | 简体中文

## 健康与就绪

- `GET /health`：只证明 HTTP 进程存活。
- `GET /ready`：要求认证 token、完整 metadata snapshot 和 scheduler 均就绪；关闭过程中返回 503。

Kubernetes 应使用 `/health` 做 liveness、`/ready` 做 readiness。启动时首次 metadata 刷新可能需要较长时间，主机量较大时应适当提高 readiness 的 failure threshold。

## 推荐监控

### Metadata

```promql
zabbix_exporter_metadata_age_seconds
```

直接观察 gauge 时，应重点告警：

- `zabbix_exporter_metadata_refresh_total{result!="success"}` 增长；
- metadata age 持续超过刷新周期；
- enabled item 或 host 数突然大幅下降。

### History

```promql
rate(zabbix_exporter_history_limit_hits_total[5m]) > 0
```

同时观察 history batch duration、result status、scheduler lag 和 queue entries。limit 命中伴随 lag 增长时，先确认 Zabbix API 容量，再调小 batch 或增加受控并发。

### Cache

```promql
zabbix_exporter_value_cache_fresh_entries
/
clamp_min(zabbix_exporter_metadata_enabled_items, 1)
```

`valid` 表示逻辑有效；`fresh` 还要求尚未达到源时间推导的硬 TTL。item 被禁用后，旧状态可能保留到 TTL，因此短时间内缓存数可略高于 enabled item 数。

### Remote Write

关注：

- `zabbix_exporter_push_queue_entries`
- `zabbix_exporter_push_batches_total{result=~"failed|queue_full|retryable|permanent"}`
- `zabbix_exporter_push_coalesced_cycles_total`
- batch series/bytes 和 slot duration

队列长期接近容量说明 Remote Write 吞吐低于产生速率。检查下游限流、网络和认证，再考虑调整 worker/queue；扩大队列只会延迟失败并增加内存占用。

## 常见问题

### 启动时报认证错误

1. API key 模式确认 `ZABBIX_API_KEY` 已注入，且展开后不是空字符串。
2. 用户名/密码模式确认两个字段同时存在。
3. 确认账号可调用 host、hostgroup、item 和 history 方法。
4. 检查代理、防火墙、证书链和 Zabbix API URL。

### `/ready` 一直返回 503

查看响应正文与日志，判断是无 token、metadata 尚未成功还是 scheduler 未启动。首次 metadata 必须完整成功；任一 item batch 失败都会保留旧快照并使首次启动失败。

### `/metrics` 中没有业务指标

检查：

- host group、host IP 和 item-key filter 是否过窄；
- item 是否启用，value type 是否为 0/3；
- history batch 是否成功；
- ValueCache fresh/valid entries 是否为 0；
- Zabbix item delay 和源时间是否导致样本已经硬过期。

### History limit 持续命中

`history_max_limit` 是一整个物理 batch 的结果上限，不是每个 item 的上限。减小 `history_batch_size` 通常比无限增大 limit 更安全；同时处理 scheduler lag，避免查询窗口持续扩大。

### Remote Write 返回 400/401/403

这些通常是永久错误，不会靠重试恢复。检查 URL、协议兼容性、Basic Auth、租户 header 要求和下游日志。当前配置只直接支持 Basic Auth，不支持自定义 header。

### Remote Write 返回 429/5xx 或网络超时

这些会按配置退避重试。持续发生时会占用 worker 并推高队列。修复下游容量或网络后，再评估 `workers`、`max_retries` 和 batch 限制。

### 修改配置没有生效

当前版本没有热重载端点。安全地滚动重启进程或 Deployment。

## 容量调整顺序

1. 记录 baseline：host/item 数、Zabbix API 延迟和错误、scheduler lag、cache coverage、CPU/RSS。
2. 优先确认 Zabbix 与 Remote Write 的真实容量瓶颈。
3. 一次只调整一个维度。
4. 增加 `history_query_concurrency` 前先观察 API 错误和延迟。
5. 增加 `workers` 前确认 Remote Write 能承受更多并发。
6. 不要用无限队列掩盖持续拥塞。

## 升级与回滚

- 使用带版本的镜像或 release 二进制，不要在生产固定使用 `latest`。
- 部署前保存旧镜像 tag、配置和 Secret 引用。
- 先在小范围验证 `/ready`、序列数量、标签、时间戳和 Remote Write 错误。
- 回滚会丢失内存缓存；旧版本启动后需要重新建立 metadata 和 ValueCache。

## 收集诊断信息

提交 Issue 前请提供：

- `zabbix-exporter --version`
- 脱敏后的配置
- Zabbix 和下游版本
- 问题时间窗口及 exporter 日志
- 相关自监控指标

删除 API key、密码、内部域名/IP、真实主机名和不必要的指标值。
