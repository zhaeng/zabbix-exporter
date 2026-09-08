# Zabbix 指标采集、缓存与推送改造设计

## 1. 文档信息

- 状态：历史设计基线（C01 后运行时已收敛为 scheduled 唯一路径）
- 日期：2026-08-24
- 适用项目：`zabbix-exporter`
- 范围：Zabbix 指标采集、metadata 管理、指标值缓存、缓存清理和 Prometheus Remote Write 推送
- 详细实施计划：[Zabbix 指标采集、缓存与推送改造实施计划](./collection-cache-push-refactoring-implementation-plan.md)
- 实施任务目录：[Implementation Tasks](./implementation_tasks/README.md)

## 2. 背景

> 当前运行与配置说明以 [README](../README.md) 和 [C01 配置迁移与版本回滚](./C01-scheduled配置迁移与版本回滚.md) 为准。本文件保留改造前问题与设计决策，文中的旧链路不是当前可选运行模式。

改造前服务的主要数据链路是：

```text
Zabbix API
    ↓
Collector 全量采集
    ↓
MetricCache 整体替换
    ↓
定时读取缓存
    ↓
Prometheus Remote Write
```

现有实现已经从 `item.get` 获取指标值改为使用 `history.get`。改造原因是生产环境中 `item.get` 存在以下问题：

- 大批量请求容易报错。
- 返回时间过长，容易拖慢整轮采集。
- 当主机和 item 数量较大时，请求耗时和返回体不可控。
- 如果把 `item.get` 放在每轮采集或每轮推送的热路径上，Zabbix 响应变慢会直接影响 Prometheus 数据时效性。

因此，本方案明确保留 `history.get` 作为指标值采集接口，不恢复使用 `item.get` 拉取 `lastvalue/lastclock`。

改造前还存在旧值持续推送的问题：当时的 `history.get` 没有查到某个 item 数据时，代码会保留 item 缓存中的上一次 `LastValue/LastClock`；转换时又可能把旧时间戳替换为当前时间。最终导致 Zabbix 已经没有新数据，但 Prometheus 仍持续收到旧值生成的新样本。

随着指标规模增长到几十万甚至上百万，改造前的整块缓存、全量扫描、一次性构造全部 Remote Write 数据的方式也会带来锁竞争、内存峰值、GC 压力和周期性流量尖峰。

## 3. 改造目标

### 3.1 正确性目标

- Zabbix 没有新数据时，旧值不能无限续命。
- 区分“查询成功但无数据”和“查询失败、状态未知”。
- 长采集周期指标允许在有效期内按分钟补点，但必须保留真实 Zabbix 来源时间。
- 短采集周期指标只推送足够新鲜的数据。
- item 或 host 删除、禁用、标签变化以及数据过期时，正确终止旧时间序列。
- 任意批次失败不能被错误地当作本轮成功空结果。

### 3.2 性能目标

- 支持几十万到百万级指标状态管理。
- 避免每分钟全量扫描缓存进行过期清理。
- 避免一次性在内存中构造全部 `Sample` 和 `TimeSeries`。
- 采集和推送错峰，降低 Zabbix 和 Remote Write 的瞬时压力。
- 缓存更新、清理和推送之间减少全局锁竞争。
- 慢批次、慢主机和慢 Remote Write 推送之间相互隔离。

### 3.3 可运维目标

- 能观察 metadata 刷新、分组调度、采集成功率、空结果、缓存年龄、过期清理和推送积压。
- 参数可配置，并可通过压测调整 batch、worker、shard 和错峰窗口。
- 改造可以分阶段上线，不要求一次性替换全部逻辑。

## 4. 非目标和约束

### 4.1 指标值采集不使用 `item.get`

指标值采集热路径统一使用 `history.get`。

```text
不允许：每轮推送 → item.get → lastvalue → Remote Write
不允许：每轮采集 → 对每台主机调用 item.get 获取 lastvalue
允许：按调度分组和批次调用 history.get
```

### 4.2 metadata 场景中的 `item.get`

Zabbix item 定义包含 `itemid`、`hostid`、`key_`、`value_type`、`delay`、状态等信息，metadata 发现仍可能需要使用 `item.get`。如果没有其他可靠的 metadata 来源，则必须对其进行严格限制：

- 只请求 metadata 最小字段，不请求 `lastvalue/lastclock`。
- 不进入每分钟采集和推送热路径。
- 低频刷新，例如 10～30 分钟一次。
- 按 host 或分页游标分批获取，禁止一次请求全部 item。
- 独立超时、限流和重试，不能阻塞 history 采集。
- 刷新失败时继续使用上一次成功的 metadata，不清理现有指标。
- 只有一次完整且成功的 metadata 刷新才能触发删除差异对账。

如果生产环境确认任何规模的 `item.get` 都不可接受，则需要把 metadata 改为外部同步、配置文件、数据库或独立低频同步任务。本方案不使用 `item.get` 代替 `history.get` 获取指标值。

### 4.3 分钟级发布目标

本方案默认 Prometheus 侧需要一分钟粒度：

- `delay <= 1m`：每分钟采集并发布最新的有效数据。
- `delay > 1m`：按 Zabbix delay 采集，在有效期内每分钟发布一次缓存值。

如果需要保留 Zabbix 10 秒、30 秒等原始分辨率，应启用 history 增量全量模式，推送 watermark 之后的所有记录，而不是只选最新记录。

## 5. 需要解决的现有问题

### 5.1 history 未命中时保留旧值

当前 item 定义缓存和指标值混在同一个结构里。本轮 history 未命中后，旧 `LastValue/LastClock` 仍被返回给下游。

改造后 metadata cache 不能保存指标值；每轮 history 返回结果必须生成独立的采集结果状态。

### 5.2 旧时间戳被改成当前时间

当前转换逻辑会在 `LastClock` 较旧时改用当前时间，导致旧值重新变成新样本。

改造后必须区分：

- `SourceTimestamp`：Zabbix history 中的原始 `clock`。
- `FetchedAt`：本次 API 请求完成时间。
- `PublishTimestamp`：发送到 Prometheus 的时间。

缓存过期只能依据 `SourceTimestamp`，不能依据 `FetchedAt` 或缓存写入时间。

### 5.3 history 批次失败被吞掉

当前部分 `history.get` 请求失败时只记录日志，外层仍可能把整轮采集视为成功。

改造后每个批次必须返回明确状态：

- 成功且有结果。
- 成功但没有新结果。
- 响应达到 limit，结果可能不完整。
- API 错误或超时。
- 响应解析错误。

失败和不完整批次不能触发该批次 item 的 miss 或删除。

### 5.4 缓存过期依据错误

当前缓存以最后一次 `SetSamples` 时间作为整体过期依据。只要旧值被重复写入，缓存就永远不会过期。

改造后每个 item 独立保存 `SourceTimestamp` 和 `ExpireAt`。

### 5.5 所有 item 使用同一采集周期

当前周期性整轮采集无法利用 Zabbix item 的 delay。长周期指标被频繁查询，短周期指标又可能不够及时。

改造后按 host、value type 和 delay 分组调度。

### 5.6 大规模下的缓存和推送问题

- 整体 `[]*model.Sample` 替换需要重复分配大切片。
- 全量扫描清理复杂度为 `O(N)`。
- 推送前先构造全部 `[]prompb.TimeSeries`，内存峰值大。
- 整分钟集中采集和推送形成尖峰。
- 一把全局锁会阻塞采集、清理和推送。

## 6. 总体架构

```text
                 ┌──────────────────────────┐
                 │ Metadata Refresher       │
                 │ host/item/inventory      │
                 └─────────────┬────────────┘
                               │ reconcile
                               ▼
                 ┌──────────────────────────┐
                 │ Metadata Cache           │
                 │ immutable definitions    │
                 └─────────────┬────────────┘
                               │ build groups
                               ▼
                 ┌──────────────────────────┐
                 │ Collection Scheduler     │
                 │ due queue + worker pool  │
                 └─────────────┬────────────┘
                               │ history.get
                               ▼
                 ┌──────────────────────────┐
                 │ Batch Result Processor   │
                 │ data/empty/error/limit   │
                 └─────────────┬────────────┘
                               │ update state
                               ▼
       ┌─────────────────────────────────────────────┐
       │ Sharded Value Cache                         │
       │ source time/value/miss/expire/version       │
       └───────────────┬─────────────────┬───────────┘
                       │                 │
              expiry wheel          publish slots
                       │                 │
                       ▼                 ▼
                delete state     ┌──────────────────┐
                                 │ Stream Publisher │
                                 └────────┬─────────┘
                                          ▼
                               Prometheus Remote Write
```

采集调度器和推送调度器完全解耦：

- 采集器只负责按照调度从 Zabbix 拉取并更新 Value Cache。
- 推送器只负责读取当前有效 Value State，生成本轮样本并推送。
- 推送器不能同步触发 Zabbix API 请求。

## 7. 核心数据模型

### 7.1 Host metadata

```go
type HostMetadata struct {
    HostID       string
    HostName     string
    HostIP       string
    Inventory    *zabbix.HostInventory
    Labels       []prompb.Label
    RefreshedAt  time.Time
    Version      uint64
}
```

### 7.2 Item metadata

```go
type ItemMetadata struct {
    ItemID       string
    HostID       string
    Key          string
    Name         string
    ValueType    string
    Delay        time.Duration
    Enabled      bool
    Labels       []prompb.Label
    RefreshedAt  time.Time
    Version      uint64
}
```

`Labels` 在 metadata 更新时生成并保持不可变，推送时复用，避免每分钟重新构造 label map。

### 7.3 采集分组

```go
type CollectionGroup struct {
    ID             string
    HostID         string
    ValueType      string
    Delay          time.Duration
    ItemIDs        []string
    NextRunAt      time.Time
    LastQueryTill  time.Time
    InFlight       bool
    Version        uint64
}
```

推荐分组键：

```text
hostID + valueType + normalizedDelay
```

### 7.4 指标值状态

```go
type ValueState struct {
    ItemID             string
    Value              float64
    SourceTimestamp    time.Time
    LastFetchAt        time.Time
    LastSuccessAt      time.Time
    Delay              time.Duration
    ExpireAt           time.Time
    ConsecutiveMisses  int
    MetadataVersion    uint64
    Version            uint64
    Valid              bool
}
```

Value Cache 使用 `itemid` 作为主键。如果一个 Zabbix item 将来需要转换为多个 Prometheus series，则缓存键升级为 `itemid + series fingerprint`。

## 8. Metadata 管理方案

### 8.1 缓存范围

- 主机列表和接口 IP。
- inventory。
- item 定义。
- item delay、value type、状态。
- 转换后的 Prometheus metric name 和 labels。
- host 到 item、group 到 item 的索引。

### 8.2 刷新与对账

metadata 刷新流程：

```text
低频拉取新 metadata
    ↓
在临时区域构建完整快照
    ↓
确认所有分页/批次成功
    ↓
与当前版本进行 diff
    ↓
原子切换 metadata 快照
    ↓
新增/删除/变更采集分组
```

只有完整刷新成功才能执行删除对账。部分批次失败时沿用当前 metadata，避免把“请求失败”误判成“大量 item 被删除”。

### 8.3 item.get 风险控制

metadata 如果使用 `item.get`，建议：

- `output` 只包含必要字段。
- 不请求 `lastvalue`、`lastclock` 和不需要的关联对象。
- 按 host 批次或稳定分页读取。
- 为 metadata 设置独立 worker 和并发上限。
- 对返回时间和返回条数设置监控。
- 失败后指数退避，不影响现有 history 调度。
- metadata 缓存可以跨刷新周期继续使用，但要设置最长允许陈旧时间并告警。

## 9. history.get 采集方案

### 9.1 分组和批次

同一批次只包含相同 host、value type 和 delay 的 item。

建议初始批次大小：

- 每批 50 个 itemid。
- 根据 Zabbix 响应时间、返回条数和响应体大小压测调整。
- 单个慢批次只影响本批次，不影响其他 host 和 group。

### 9.2 查询时间窗口

每个 group 或 batch 保存 `LastQueryTill`：

```text
time_from = LastQueryTill - overlap
time_till = now
```

`overlap` 用于容忍调度误差和 history 延迟落库，建议默认：

```text
max(60 秒, delay × 20%)
```

返回结果按 `itemid + clock + ns` 去重，并为每个 item 选择最新记录。缓存中的 `SourceTimestamp` 用于判断结果是否真正更新。

首次启动没有 watermark 时使用有限启动窗口：

```text
短周期：max(2 × delay, 5 分钟)
长周期：2 × delay + grace
```

启动窗口只能用于找到最近有效记录，超过最大允许年龄的 history 不能进入 Value Cache。

### 9.3 批量查询与“每个 item 只取最新点”

`history.get` 查询多个 itemid 时，会返回查询时间窗口内这些 item 的多条历史记录。Zabbix API 没有提供“一个批量请求中，每个 item 分别只返回最新一条”的参数。

`sortfield=clock`、`sortorder=DESC` 和 `limit` 作用于整个响应结果，不是分别作用于每个 item。因此不能通过下面的方式保证每个 item 返回一条：

```text
itemids = 50 个
limit = 50
```

如果某个高频 item 在窗口内有很多记录，它可能占据多个返回位置，导致其他 item 完全没有出现在响应中。

本项目采用“批量获取、Exporter 端只保留每个 item 最新点”的方式：

```go
latest := make(map[string]HistoryItem)

for _, record := range history {
    current, exists := latest[record.ItemID]
    if !exists || record.Clock > current.Clock ||
        (record.Clock == current.Clock && record.NS > current.NS) {
        latest[record.ItemID] = record
    }
}
```

短周期指标默认只把 `latest[itemid]` 写入 Value Cache 和推送队列，窗口内更早的记录直接丢弃。因此 Prometheus 最终只收到每个 item 本轮查询到的最新点，但 Zabbix API 响应本身仍可能包含多个时间点。

这种方式避免了“每个 item 单独发一个 `history.get + limit=1`”产生的大量 API 请求，是当前性能和结果完整性之间的折中。

为了减少返回的旧记录：

- 按相同 delay 分组。
- 查询窗口从上次 `LastQueryTill - overlap` 开始，不使用固定一小时或六小时大窗口。
- `output` 只请求 `itemid`、`clock`、`ns` 和 `value`。
- 批次大小保持可控。
- 解析后立即聚合为每 item 最新记录，不把全部 history 长期保存在缓存中。

如果未来需要保留短周期原始分辨率，可以增加配置切换为“推送 watermark 后全部 history 点”；默认模式仍为每 item 最新点。

### 9.4 limit 和结果完整性

`history.get` 的 `limit` 对整个批次生效。某些活跃 item 可能占满 limit，导致其他 item 看起来没有返回。

因此：

- 同批次 item 必须具有相近 delay。
- 查询窗口要尽量窄，不再固定查询过去 1 小时或 6 小时。
- limit 根据批次 item 数量和窗口内理论记录数计算。
- 如果返回条数达到 limit，将批次标记为“可能截断”。
- 可能截断时拆小批次重试，不能对未返回 item 直接记 miss。

建议公式：

```text
expected_per_item = ceil(window / delay) + overlap_reserve
limit = item_count × expected_per_item
```

同时配置单次最大 limit，超过时优先缩小 batch，而不是无限增大响应体。

### 9.5 采集结果状态

```go
type BatchStatus int

const (
    BatchSuccess BatchStatus = iota
    BatchEmpty
    BatchPossiblyTruncated
    BatchAPIError
    BatchDecodeError
)
```

处理规则：

| 结果 | Value Cache 处理 |
|---|---|
| 成功且 item 有更新 clock | 更新值、来源时间、过期时间，miss 清零 |
| 成功但 item 没有新 clock | miss 加一，不刷新来源时间 |
| 成功但 item 未返回 | 在确认结果完整后 miss 加一 |
| 返回条数达到 limit | 拆批重试，不记 miss |
| API/解析错误 | 保留原状态，不记 miss |
| metadata 确认 item 删除/禁用 | 立即从 Value Cache 删除并停止发布 |

### 9.6 调度和错峰

- 使用最小堆或调度时间轮维护 `NextRunAt`。
- 使用固定大小 worker pool，不为每个 group 创建永久 goroutine。
- group 设置 `InFlight`，上一轮未完成时不重复入队。
- 采集 offset 使用稳定 hash：

```text
offset = hash(groupID) % schedulingWindow
```

- 服务重启后仍保持相对稳定的错峰位置。
- 启动时对全部 group 做限速 bootstrap，禁止瞬间请求所有主机。

## 10. 长短周期指标发布策略

### 10.1 短周期指标：delay 小于等于一分钟

- 每分钟调度采集一次。
- 默认只选查询窗口内每个 item 的最新记录。
- 使用 Zabbix 原始 `clock` 作为样本时间戳。
- 不把超过新鲜度上限的旧值改成当前时间。
- 如果需要原始高分辨率，推送 watermark 后的全部 history 记录。

短周期默认最大年龄：

```text
max(90 秒, 2 × delay)
```

### 10.2 长周期指标：delay 大于一分钟

- 按 Zabbix delay 调度 `history.get`。
- 获取到新值后写入 Value Cache。
- 值仍然有效时，每分钟发布一次缓存值。
- 发布样本使用当前发布周期时间戳，实现 sample-and-hold。
- Value Cache 中的 `SourceTimestamp` 保持为 Zabbix 原始 `clock`，不能随推送更新。

示例：

```text
Zabbix 10:00 产生 value=100

Prometheus：
10:00 value=100
10:01 value=100
10:02 value=100
10:03 value=100
```

长周期默认最大年龄：

```text
grace = max(30 秒, delay × 20%)
max_age = 2 × delay + grace
```

### 10.3 sample-and-hold 的限制

使用当前时间重复发布缓存值会生成合成样本：

- 适合状态型 gauge。
- 会隐藏原始指标更新频率。
- counter 会呈阶梯状，需要确认 `rate()` 查询是否符合预期。
- 事件型、瞬时型指标不应默认补点。

后续可在 metadata 中增加 `HoldPolicy`，允许按 metric pattern 配置：

```text
hold：长周期内每分钟补点
source_only：只推送 Zabbix 原始点
drop_when_missing：本轮没有新值就停止发布
```

## 11. Value Cache 设计

### 11.1 分片缓存

Value Cache 按 itemid hash 拆成 64～256 个 shard：

```go
type ValueShard struct {
    mu      sync.RWMutex
    values  map[string]*ValueState
}

type ValueCache struct {
    shards []*ValueShard
}
```

采集、清理和推送只锁定目标 shard，避免一把全局锁阻塞百万级指标。

### 11.2 批次原子更新

采集批次处理时：

- 先在锁外解析和去重 history。
- 按 shard 对更新进行分组。
- 每个 shard 一次加锁完成批量更新。
- 推送器只能看到某个 shard 更新前或更新后的状态，不能看到半条状态。

### 11.3 不重复保存 labels

Value State 不复制完整 label map，只引用不可变 metadata 或保存 metadata version。Prometheus labels 在 metadata cache 中预生成并复用。

## 12. 缓存清理方案

### 12.1 失效条件

以下情况立即或延迟清理 Value State：

| 场景 | 清理规则 |
|---|---|
| metadata 确认 item 删除或禁用 | 立即清理并停止发布 |
| host 被确认删除 | 级联清理该 host 的全部 item |
| metric name 或 labels 变化 | 旧 series 清理，新 series 重新创建 |
| history 成功但连续无新数据 | 达到 miss 和时间条件后清理 |
| history API 失败 | 不立即清理，但仍受硬 TTL 限制 |
| 来源时间超过 `ExpireAt` | 强制清理 |

推荐普通失效条件：

```text
ConsecutiveMisses >= 2
并且
now > SourceTimestamp + delay + grace
```

推荐硬失效条件：

```text
now >= ExpireAt
```

### 12.2 过期时间轮

禁止每分钟全量扫描全部 Value State。使用分钟级过期桶或分层时间轮：

```go
type ExpiryEntry struct {
    ItemID   string
    Version  uint64
}
```

更新 Value State 时：

1. 计算新的 `ExpireAt`。
2. `Version++`。
3. 将 `ItemID + Version` 加入对应的过期桶。

清理任务每分钟只处理当前到期桶。若 entry version 与当前 state version 不一致，说明这是旧的过期任务，直接跳过。

清理复杂度从 `O(全部指标数)` 降为 `O(当前到期指标数)`。

### 12.3 删除后的远端行为

本项目不发送 Prometheus stale marker。Value State 被删除后，Publisher 从下一轮开始不再发送该 series。

需要接受以下语义：Remote Write 接收端可能在自身 lookback 或查询窗口内继续返回最后一个历史样本，直到该样本自然变旧。Exporter 只保证不再生成新的当前时间样本，不负责主动向远端写入失效标记。

## 13. 推送方案

### 13.1 推送数据来源

推送器只从 Value Cache 取值：

```text
Value Cache.PublishBatch(slot, now)
```

推送器不调用 Zabbix API，也不改变 `SourceTimestamp`。

### 13.2 分钟内错峰

每个 series 使用稳定 hash 分配推送槽：

```text
slot = hash(series fingerprint) % slotCount
```

例如配置 12 个 slot，则每 5 秒推送一个 slot，每个 series 每分钟发布一次。百万指标不会在整分钟边界同时构造和发送。

### 13.3 流式批处理

禁止先生成全部 `[]prompb.TimeSeries` 再切批。新的流程为：

```text
读取当前 slot 的部分 Value State
    ↓
生成 2,000～10,000 条 TimeSeries
    ↓
Marshal + Snappy
    ↓
发送当前批次
    ↓
释放批次内存
    ↓
继续下一批
```

批次同时受两个条件限制：

- 最大 series 数量。
- 最大未压缩或压缩字节数。

### 13.4 单 endpoint 队列和顺序

当前部署只有一个 Remote Write endpoint：

- 使用一个独立的有界推送队列和固定 worker pool。
- 同一 series 使用稳定 shard，尽量保证时间戳发送顺序。
- 不允许无限积累完整推送轮次。
- gauge 普通值积压时可合并到最新状态。
- 重试应复用原批次时间戳，避免同一逻辑批次产生不同时间。

### 13.5 时间戳单调性

长周期补点使用发布周期时间戳。需要避免：

- NTP 回拨导致时间戳倒退。
- 上一轮慢请求晚于下一轮到达。
- Remote Write 重试产生乱序。

可以按推送周期生成固定 `cycleTimestamp`，并在 series shard 内确保不小于上一发布时间。

## 14. 性能设计

### 14.1 百万级主要成本

百万指标每分钟发布一次，平均约为 1.67 万 series/秒。主要成本包括：

- history API 请求和 JSON 解码。
- metadata 和 Value State 内存。
- labels、protobuf 对象分配。
- Snappy 压缩。
- Remote Write 网络流量。
- 高基数时间序列在接收端的存储压力。

缓存全量扫描只是其中一个问题，推送编码和网络往往更重。

### 14.2 优化原则

- Value Cache 分片。
- 清理使用过期时间轮。
- labels 预生成并复用。
- history 解析在锁外完成。
- 采集和推送使用固定 worker pool。
- 推送按 slot 错峰。
- Remote Write 流式批处理。
- 队列全部有界，提供背压。
- 避免在热路径复制百万级 slice。
- 对 `item_name`、`item_key` 等长标签提供开关，减少传输体积。

### 14.3 metadata 大规模刷新

- metadata 低频刷新并独立限流。
- 构建临时快照后原子切换，读路径不长时间持锁。
- host/item 索引只在 metadata 版本变化时重建。
- 避免每轮 history 采集重新构造分组和 labels。

## 15. 故障处理矩阵

| 故障 | 采集 | 缓存 | 推送 |
|---|---|---|---|
| 单个 history 批次超时 | 本批失败，调度重试 | 不记 miss，不刷新时间 | 有效期内继续发布缓存 |
| history 返回空 | 标记成功空结果 | item 记 miss | 未过期时按策略发布 |
| history 达到 limit | 拆批重试 | 不记 miss | 保持当前状态 |
| Zabbix token 失效 | 刷新 token | 不清缓存 | 有效期内继续发布 |
| metadata 刷新部分失败 | 不切换 metadata 版本 | 保留旧 metadata/value | 正常推送 |
| metadata 完整刷新确认删除 | 删除调度和状态 | 删除 Value State | 停止发送该 series |
| Remote Write 失败 | 不影响采集 | Value Cache 不变 | 有界重试或下一周期重新发布 |
| 服务重启 | 重新加载 metadata 并错峰启动 | 内存 value 丢失 | 获取到有效 history 后恢复 |

默认不持久化 Value Cache。服务重启后重新从 Zabbix 获取最近有效 history，比恢复未知年龄的旧值更安全。如果未来要求无缝重启，需要持久化 `SourceTimestamp`、value、metadata version 和 expire time，并在恢复时重新校验 TTL。

## 16. 建议配置

以下为设计建议，字段名称可在实现阶段调整：

```yaml
collector:
  workers: 32
  history_query_concurrency: 4
  history_batch_size: 50
  history_query_timeout: 30s
  query_overlap: 60s
  short_interval_threshold: 60s
  startup_spread_window: 60s
  metadata_refresh_interval: 20m
  metadata_workers: 2
  metadata_batch_hosts: 50

cache:
  shards: 256
  expiry_tick: 1m
  miss_threshold: 2
  short_min_max_age: 90s
  long_max_age_factor: 2.0
  long_grace_factor: 0.2

prometheus:
  push:
    interval: 60s
    spread_slots: 12
    max_samples_per_send: 5000
    max_batch_bytes: 4MB
    workers: 4
    queue_capacity: 100
```

具体值必须通过实际 Zabbix、Exporter 和 Remote Write 接收端压测确定。

## 17. 可观测性指标

建议新增：

```text
zabbix_exporter_metadata_refresh_total{status}
zabbix_exporter_metadata_refresh_duration_seconds
zabbix_exporter_metadata_age_seconds
zabbix_exporter_collection_groups
zabbix_exporter_collection_group_runs_total{status}
zabbix_exporter_history_batches_total{status}
zabbix_exporter_history_response_items
zabbix_exporter_history_response_limit_hits_total
zabbix_exporter_value_cache_entries
zabbix_exporter_value_cache_expired_total{reason}
zabbix_exporter_value_cache_misses_total
zabbix_exporter_push_queue_entries
zabbix_exporter_push_batches_total{status}
zabbix_exporter_push_batch_series
zabbix_exporter_push_batch_bytes
zabbix_exporter_scheduler_lag_seconds
```

不建议默认为每个 item 暴露一套内部健康指标，否则会使内部指标基数翻倍。内部指标优先按 host、delay bucket 或 group 维度聚合。

## 18. 代码结构建议

```text
internal/metadata/
    cache.go
    refresher.go
    reconciler.go

internal/scheduler/
    scheduler.go
    group.go
    worker_pool.go

internal/collector/
    group_collector.go
    batch_result.go

internal/cache/
    value_cache.go
    shard.go
    expiry_wheel.go

internal/promwrap/
    publisher.go
    batch_encoder.go
    push_worker.go
```

现有代码的主要调整点：

- `internal/zabbix/client.go`
  - 保留 `history.get` 值采集路径。
  - 删除 history 未命中时继承旧 `LastValue/LastClock` 的逻辑。
  - history 批次返回结构化状态，不再只打印错误。
  - metadata 的 `item.get` 与 history 热路径隔离。
- `internal/transformer/transformer.go`
  - 不再把过期 Zabbix 时间戳自动替换为当前时间。
  - 长周期补点时间戳由 Publisher 生成。
- `internal/cache/cache.go`
  - 整块 sample slice 改为分片 Value State Cache。
  - 增加过期时间轮。
- `internal/collector/collector.go`
  - 从整轮全量采集改为按 group/batch 采集。
  - 明确成功、有数据、空、截断、失败状态。
- `internal/promwrap/push.go`
  - 只从 Value Cache 读取。
  - 按 slot 和 batch 流式编码推送。
  - 单 endpoint 有界队列和失败处理。
- `cmd/server/main.go`
  - 分别启动 metadata refresher、collection scheduler、expiry worker 和 publisher。

## 19. 分阶段实施计划

### 阶段一：修复旧值正确性

- metadata 和 value 分离。
- history 未命中时不继承旧值。
- 保留 Zabbix 原始 `clock`。
- 区分 history 空结果和 API 错误。
- 为旧值问题增加回归测试。

验收结果：Zabbix 停止产生数据后，指标不会永久以当前时间继续推送。

### 阶段二：引入 Value State 和长短周期策略

- 建立按 itemid 的 Value Cache。
- 实现短周期 freshness。
- 实现长周期 sample-and-hold。
- 实现 miss、grace 和硬 TTL。

验收结果：长周期指标一分钟一个点，超过允许年龄后自动停止。

### 阶段三：采集调度改造

- metadata 生成采集 group。
- 最小堆或时间轮调度。
- worker pool、batch、in-flight 防重。
- history 查询窗口、overlap、limit 截断检测和拆批重试。
- 启动和运行时错峰。

验收结果：不同 delay 的 item 按各自周期查询，慢 host 不阻塞全局。

### 阶段四：缓存清理

- Value Cache 分片。
- 过期时间轮。
- metadata 删除对账。

验收结果：不做全量缓存扫描，删除、禁用和过期 series 从下一轮开始停止发布。

### 阶段五：推送性能改造

- 推送 slot 错峰。
- 流式 batch 构造和压缩。
- 单 endpoint 有界队列。
- 同 series 顺序和时间戳单调性保护。

验收结果：百万级推送不存在整分钟尖峰，内存峰值受 batch 上限控制。

### 阶段六：压测和参数定型

- 10 万、50 万、100 万指标分级压测。
- history 查询耗时、Zabbix QPS 和 limit 截断测试。
- Value Cache 内存、锁等待和 GC 测试。
- Remote Write 吞吐、压缩比、失败重试和积压测试。
- metadata `item.get` 分批规模和刷新耗时测试。

## 20. 测试计划

### 20.1 正确性测试

- 上一轮有值、本轮 history 为空，不得刷新 `SourceTimestamp`。
- history API 失败不得记 miss。
- history 返回条数达到 limit 时不得清理缺失 item。
- 长周期值在有效期内每分钟发布。
- 长周期值超过 `ExpireAt` 后停止发布。
- 短周期旧值不得改成当前时间发布。
- metadata 部分刷新失败不得删除任何 item。
- metadata 完整刷新确认删除后清理 value 和调度任务。
- item 删除后重新出现时，能够以新的 metadata version 恢复采集和发布。

### 20.2 并发和稳定性测试

- `go test -race` 覆盖 metadata、scheduler、cache、expiry 和 publisher。
- 同一 group 不得并发采集。
- 清理和更新同一 item 时 version 判断正确。
- 推送读取期间 metadata 版本切换不产生半更新 labels。
- Remote Write 变慢时队列有界且不会阻塞采集。

### 20.3 性能测试

- 建立百万 Value State 的内存基准。
- 每秒一万级批量更新基准。
- 时间轮单桶大量过期基准。
- 每批 2,000、5,000、10,000 series 的编码和压缩基准。
- 长标签开关对请求体大小和 CPU 的影响。
- 12/60 个推送 slot 的流量平滑度对比。

## 21. 验收标准

### 21.1 功能验收

- Zabbix history 没有新数据时，旧值不会无限刷新时间戳。
- 长周期指标在配置有效期内保持一分钟粒度。
- 过期、删除、禁用指标能够停止发布。
- history 部分失败不会误删缓存。
- metadata `item.get` 故障不会中断正常 history 采集和已有数据推送。

### 21.2 性能验收

- 缓存清理耗时与当前到期量相关，而不是与总指标量线性相关。
- 推送峰值内存受单批大小和队列容量约束。
- 百万级指标不在整分钟边界集中构造全部 TimeSeries。
- worker 和队列数量有固定上限，不随 group、item 或失败次数无限增长。
- 在目标负载下无持续 GC 抖动、无无界队列、无明显调度延迟累积。

### 21.3 可运维验收

- 能从内部指标判断 metadata 是否陈旧。
- 能区分 history 空结果、截断、API 错误和解析错误。
- 能看到 cache entries、过期原因和 push backlog。
- 关键失败均有聚合日志，避免每个 item 一条日志造成日志风暴。

## 22. 待确认项

- 哪些指标允许 sample-and-hold，是否默认只对 gauge 开启。
- 短周期指标只取最新点，还是需要保留全部 history 原始点。
- 生产 Zabbix 对单批 50 个 itemid 的稳定响应上限。
- metadata `item.get` 可以接受的 host 批次、超时和刷新周期。
- flexible delay、scheduled interval 和 user macro 的实际格式及解析策略。
- 单机目标最大指标量、CPU、内存和网络预算。
