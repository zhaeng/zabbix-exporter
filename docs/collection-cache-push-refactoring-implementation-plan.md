# Zabbix 指标采集、缓存与推送改造实施计划

## 1. 文档信息

- 状态：历史实施计划（C01 后运行时已收敛为 scheduled 唯一路径）
- 日期：2026-08-24
- 关联设计：[Zabbix 指标采集、缓存与推送改造设计](./collection-cache-push-refactoring-design.md)
- 任务拆分：[Implementation Tasks](./implementation_tasks/README.md)
- 适用项目：`zabbix-exporter`
- 实施范围：metadata、`history.get` 采集、调度、Value Cache、过期清理和单端点 Remote Write

## 2. 文档目的

> 本文保留分阶段实施和历史门禁设计，不是当前运行配置说明。当前程序不再支持 pipeline/Publisher 选择；请以 [README](../README.md) 和 [C01 配置迁移与版本回滚](./C01-scheduled配置迁移与版本回滚.md) 为准。本地完成 C01 不代表已经生产切换或通过稳定观察。

本文档把总体设计拆解为可执行的工程任务，明确：

- 固定技术决策和不再讨论的边界。
- 每个阶段修改哪些包和文件。
- 新增接口、数据结构和状态流转。
- 阶段之间的依赖关系。
- 每个阶段需要完成的测试和验收条件。
- 如何在不影响现网数据的情况下灰度、切换和回滚。
- 百万级指标场景下需要完成的性能验证。

本文档不直接修改业务代码；实际编码按阶段提交，每个阶段应保持可编译、可测试、可回滚。

## 3. 已确认的技术决策

以下内容作为实施前提，不在开发过程中重新切换方案。

### 3.1 Zabbix API

- 指标值采集只使用 `history.get`。
- 不使用 `item.get` 获取 `lastvalue/lastclock`。
- `item.get` 只允许用于低频 metadata 发现，并且只请求最小字段。
- metadata `item.get` 失败不能中断已有 history 调度和推送。
- history 查询按 host、value type、delay 分组和分批。

### 3.2 history 数据选择

- `history.get` 可以返回一个 item 的多个时间点。
- 默认模式下，短周期和长周期指标都只选本批次每个 item 最新的 `clock + ns` 记录。
- 短周期指标默认不保留查询窗口内更早的 history 点。
- `limit` 是批次全局限制，不能用 `limit=item 数量` 假设每个 item 返回一条。
- 命中 limit 的批次必须拆分重试，不能把未返回 item 记为无数据。

### 3.3 发布拓扑

- 当前只有一个 Remote Write endpoint。
- 当前只有一个 Exporter 实例负责这批时间序列。
- 不实现多 Exporter 选主、分片归属和去重。
- 不发送 Prometheus stale marker。
- 本地缓存删除后只停止产生新样本，远端历史值自然过期。

### 3.4 长短周期语义

- `delay <= 1m` 为短周期指标。
- 短周期指标按分钟采集，默认只发布本轮最新的新鲜点，使用 Zabbix 原始时间戳。
- `delay > 1m` 为长周期指标。
- 长周期指标按 delay 采集，在缓存有效期内每分钟补点，发布时使用当前周期时间戳。
- 缓存始终保存 Zabbix 原始 `SourceTimestamp`，推送不能改变它。

### 3.5 缓存和清理

- metadata cache 和 value cache 完全分离。
- Value Cache 按 item 维护状态，不再整体替换 sample slice。
- API 错误不等同于空结果。
- 缓存过期依据 `SourceTimestamp`，不依据最后请求时间或缓存写入时间。
- 百万级场景不做每分钟全量扫描清理，使用过期时间轮或时间桶。

## 4. 实施原则

### 4.1 正确性优先

第一阶段先停止旧值无限续命，再逐步引入调度和性能优化。不能为了性能继续保留错误时间戳或吞掉 history 批次错误。

### 4.2 每阶段独立可验收

每个阶段必须满足：

- `go test ./...` 可以执行；需要外部环境的测试必须有明确 build tag 或跳过条件。
- 新逻辑有单元测试或集成测试覆盖。
- 不依赖后续阶段才能保证当前阶段的数据正确性。
- 有明确的配置开关或回滚方式。

### 4.3 热路径有界

以下资源必须有固定上限：

- Zabbix history 并发数。
- metadata 并发数。
- 调度队列长度。
- Remote Write 队列长度。
- 单批 item 数量。
- 单批 series 数量和字节数。
- 每轮重试次数。

### 4.4 不在锁内执行 I/O

- 不在缓存锁内调用 Zabbix API。
- 不在缓存锁内执行 JSON 解码、protobuf marshal、Snappy 压缩或 HTTP 请求。
- 锁内只进行 map 查询、状态比较和小批量状态替换。

## 5. 目标模块和代码结构

建议逐步形成以下模块：

```text
internal/metadata/
    types.go
    cache.go
    refresher.go
    reconciler.go

internal/scheduler/
    group.go
    scheduler.go
    worker_pool.go

internal/collector/
    history_reader.go
    history_reducer.go
    group_collector.go
    batch_result.go

internal/cache/
    value_state.go
    value_cache.go
    shard.go
    expiry_wheel.go

internal/promwrap/
    publisher.go
    publish_slot.go
    batch_encoder.go
    push_worker.go
```

迁移期间可以保留原文件和旧入口，通过配置选择 legacy 或新 pipeline。完成切换后再删除旧代码。

## 6. 核心接口草案

接口先稳定，再进行具体实现，减少各阶段互相耦合。

### 6.1 MetadataStore

```go
type MetadataStore interface {
    Snapshot() *MetadataSnapshot
    ApplySnapshot(next *MetadataSnapshot) MetadataDiff
    Item(itemID string) (*ItemMetadata, bool)
    Group(groupID string) (*CollectionGroup, bool)
}
```

`MetadataSnapshot` 为只读结构。刷新器在锁外构建完整新版本，确认成功后原子切换。

### 6.2 HistoryReader

```go
type HistoryQuery struct {
    GroupID   string
    ItemIDs   []string
    ValueType int
    TimeFrom  time.Time
    TimeTill  time.Time
    Limit     int
}

type HistoryBatchResult struct {
    Query        HistoryQuery
    Status       BatchStatus
    Latest       map[string]zabbix.HistoryItem
    Returned     int
    LimitHit     bool
    StartedAt    time.Time
    FinishedAt   time.Time
    Err          error
}

type HistoryReader interface {
    QueryLatest(ctx context.Context, query HistoryQuery) HistoryBatchResult
}
```

`HistoryReader` 负责 API 调用、响应解析和每 item 最新点聚合，不直接更新缓存。

### 6.3 ValueStore

```go
type ValueStore interface {
    ApplyBatch(result HistoryBatchResult, metadata *MetadataSnapshot) ApplyResult
    MarkBatchMiss(itemIDs []string, fetchedAt time.Time)
    DeleteItems(itemIDs []string, reason DeleteReason)
    ExpireDue(now time.Time) ExpireResult
    ReadPublishBatch(slot int, cursor PublishCursor, now time.Time, limit int) PublishPage
    AckPublished(acks []PublishAck)
    Len() int
}
```

### 6.4 Scheduler

```go
type Scheduler interface {
    ApplyMetadataDiff(diff MetadataDiff)
    Start(ctx context.Context)
    Stop()
}
```

### 6.5 Publisher

```go
type Publisher interface {
    Start(ctx context.Context)
    Stop()
    PublishSlot(ctx context.Context, slot int, cycleTimestamp time.Time) error
}
```

## 7. Value State 和状态机

### 7.1 数据结构

```go
type ValueState struct {
    ItemID                 string
    Value                  float64
    SourceTimestamp        time.Time
    SourceNS               int64
    LastFetchAt            time.Time
    LastSuccessAt          time.Time
    ConsecutiveMisses      int
    Delay                  time.Duration
    ExpireAt               time.Time
    MetadataVersion        uint64
    ValueVersion           uint64
    LastPublishedVersion   uint64
    LastPublishedTimestamp time.Time
    PublishSlot            uint16
    Valid                  bool
}
```

### 7.2 三类时间

| 字段 | 来源 | 用途 |
|---|---|---|
| `SourceTimestamp` | Zabbix history `clock/ns` | 判断新旧和缓存过期 |
| `LastFetchAt` | Exporter 请求完成时间 | 观测 API 活跃度，不用于续期 |
| `LastPublishedTimestamp` | Remote Write 成功时间 | 防止重复和乱序 |

### 7.3 状态流转

```text
不存在
  └─ history 返回有效新值 → Valid

Valid
  ├─ history 返回更大 clock/ns → 更新 ValueVersion，miss=0
  ├─ 成功但无新值 → miss+1
  ├─ API 错误 → 保持值和 miss
  ├─ metadata 删除/禁用 → Deleted
  └─ 到达 ExpireAt → Expired

Deleted / Expired
  └─ 后续 history 返回有效新值且 metadata 仍存在 → 重新创建 Valid
```

### 7.4 新值比较

```go
func newer(record zabbix.HistoryItem, state *ValueState) bool {
    if int64(record.Clock) != state.SourceTimestamp.Unix() {
        return int64(record.Clock) > state.SourceTimestamp.Unix()
    }
    return int64(record.NS) > state.SourceNS
}
```

同一个 `clock/ns` 重复返回时，不更新 `LastSuccessAt`、`ExpireAt` 或 `ValueVersion`。

## 8. 阶段零：建立基线和保护性测试

### 8.1 目的

在重构前固定当前可复现行为，建立性能和正确性基线。

### 8.2 任务

- 记录当前 1 万、10 万指标下的：
  - 单轮 Collect 耗时。
  - `history.get` 请求数和平均/最大响应时间。
  - 缓存内存。
  - Remote Write 编码耗时、请求大小和推送耗时。
  - GC 次数和暂停时间。
- 整理需要外部 Zabbix 的测试，增加显式环境变量或 build tag，避免 `go test ./...` 长时间挂起。
- 增加旧值复现测试：
  - 第一轮 history 有数据。
  - 第二轮 history 返回空。
  - 验证当前代码会保留旧值，用于证明修复效果。
- 增加 history 多点测试：同一个 item 返回多个 clock/ns。
- 增加 history 全局 limit 截断测试。

### 8.3 涉及文件

- `internal/zabbix/client_test.go`
- `internal/collector/collector_test.go`
- `internal/transformer/transformer_test.go`
- 新增 benchmark 文件，例如 `internal/cache/cache_benchmark_test.go`

### 8.4 交付物

- 当前行为测试。
- 基线压测记录。
- 可重复执行的 mock Zabbix history server。

### 8.5 验收条件

- 能稳定复现旧值问题。
- 能模拟多 item、多 history 点、空结果、limit 截断和 API 错误。
- 不配置外部 Zabbix 时，单元测试不会无限等待。

## 9. 阶段一：修复 history 结果语义

### 9.1 目的

在不引入新调度器的情况下，先保证 history 返回值处理正确。

### 9.2 任务一：结构化批次结果

修改 `internal/zabbix/client.go`：

- `GetHistory` 保持底层 API 能力。
- 新增 `QueryHistoryBatch` 或在 collector 层封装 `HistoryReader`。
- 返回 `BatchStatus`、`Returned`、`LimitHit` 和错误。
- API 错误不能只打印日志后返回空结果。
- 请求显式指定最小 output：`itemid`、`clock`、`ns`、`value`。

### 9.3 任务二：每 item 最新点聚合

新增 `internal/collector/history_reducer.go`：

```go
func ReduceLatest(records []zabbix.HistoryItem) map[string]zabbix.HistoryItem
```

规则：

- 先比较 `clock`。
- `clock` 相同时比较 `ns`。
- 相同 `clock/ns` 保留一个。
- 无效数值不进入 Value Cache，但要记录解析计数。

### 9.4 任务三：移除旧值继承

修改 `internal/zabbix/client.go` 当前 `GetItems` 行为：

- item metadata 缓存中不再长期写入 `LastValue/LastClock`。
- 本轮 latest map 中不存在的 item 不返回旧值。
- 后续阶段完成后删除 `Item.LastValue/LastClock` 在热路径中的依赖。

### 9.5 任务四：limit 截断保护

- `len(records) >= limit` 时将结果标记为 `BatchPossiblyTruncated`。
- 先将 batch 二分后重试。
- 达到最小 batch 仍截断时：
  - 扩大 limit 到配置上限，或缩短时间窗口重试。
  - 仍失败则把本批记为错误。
  - 不对未返回 item 记 miss。

### 9.6 测试

- 多 item、每 item 多点，只保留各自最新点。
- 相同 clock 不同 ns，保留最大 ns。
- 全局排序后高频 item 占满 limit，触发拆批。
- API 错误不会转换成空成功。
- history 空响应不会带出上一轮值。

### 9.7 验收条件

- history 空响应后不再输出旧 `LastValue`。
- 短周期每 item 每轮最多生成一个最新点。
- 截断批次不会误判缺失 item。

## 10. 阶段二：Metadata Cache 与值彻底分离

### 10.1 目的

建立稳定的 host/item 定义缓存，为分组调度和 Value Cache 提供只读 metadata。

### 10.2 MetadataSnapshot

新增：

- `internal/metadata/types.go`
- `internal/metadata/cache.go`

快照包含：

```go
type MetadataSnapshot struct {
    Version       uint64
    CreatedAt     time.Time
    Hosts         map[string]*HostMetadata
    Items         map[string]*ItemMetadata
    HostItems     map[string][]string
    Groups        map[string]*CollectionGroup
}
```

### 10.3 metadata 刷新

新增 `internal/metadata/refresher.go`：

- host/inventory 定期刷新。
- item metadata 使用低频、分批 `item.get`。
- 只请求：`itemid`、`hostid`、`key_`、`name`、`value_type`、`delay`、`status`、`state`。
- 不请求 `lastvalue/lastclock`。
- metadata worker 与 history worker 分开。
- 全部分页/批次成功后才生成新快照。
- 任一必要批次失败则放弃本次快照，不做删除对账。

### 10.4 metadata diff

新增 `internal/metadata/reconciler.go`，输出：

```go
type MetadataDiff struct {
    AddedItems       []string
    RemovedItems     []string
    ChangedItems     []string
    AddedGroups      []string
    RemovedGroups    []string
    ChangedGroups    []string
    RemovedHosts     []string
}
```

变更判定包含：

- delay。
- value type。
- metric name。
- labels。
- enabled/status。
- host IP 和 inventory 相关标签。

### 10.5 delay 解析

- 复用并扩展当前 `parseItemDelay`。
- 明确处理秒、分、时、天、周单位。
- flexible interval、scheduled interval 和 user macro 无法解析时：
  - 不静默假设为真实 60 秒。
  - 标记解析状态。
  - 使用配置的保守 fallback，并增加监控和聚合日志。

### 10.6 测试

- 完整刷新成功后原子切换。
- 部分 item.get 批次失败不切换版本。
- 新增、删除、delay 变化、label 变化 diff 正确。
- metadata 快照读取并发安全。
- 无法解析 delay 时使用 fallback 并记录计数。

### 10.7 验收条件

- history 热路径不再构造或更新 item metadata。
- metadata `item.get` 失败不影响已有 history 采集。
- metadata 完整刷新才能删除 item。

## 11. 阶段三：实现分片 Value Cache

### 11.1 目的

用按 item 状态缓存替换整体 sample slice，并落地长短周期语义。

### 11.2 文件

- `internal/cache/value_state.go`
- `internal/cache/shard.go`
- `internal/cache/value_cache.go`
- `internal/cache/value_cache_test.go`

### 11.3 分片设计

- 默认 256 shards，可配置为 2 的幂。
- `shard = fingerprint(itemID) & (shardCount-1)`。
- 每个 shard 独立 `RWMutex` 和 map。
- map 初始容量根据 metadata 中该 shard 的 item 数预估。

### 11.4 ApplyBatch

处理顺序：

1. 在锁外解析 history 数值。
2. 按 itemid 找 metadata。
3. 过滤不存在、禁用或不支持 value type 的 item。
4. 按 shard 将更新分组。
5. 每个 shard 一次加锁批量比较 `clock/ns`。
6. 新值更新 Value State、`ValueVersion++`、miss 清零。
7. 相同或旧 clock 不刷新 `ExpireAt`。

### 11.5 长短周期过期规则

短周期：

```go
maxAge := max(90*time.Second, 2*delay)
expireAt := sourceTimestamp.Add(maxAge)
```

长周期：

```go
grace := max(30*time.Second, delay/5)
expireAt := sourceTimestamp.Add(2*delay + grace)
```

### 11.6 miss 规则

- 只有完整成功批次中的缺失 item 才增加 miss。
- API 错误、解析错误和截断批次不增加 miss。
- 同 clock/ns 重复返回视为没有新值，可以增加 miss，但不刷新有效期。
- `ConsecutiveMisses >= missThreshold` 且超过预期更新时间后使状态失效。
- 硬 TTL 到达后不考虑 miss，直接失效。

### 11.7 发布状态

短周期指标：

- 只有 `ValueVersion > LastPublishedVersion` 时进入发布。
- 推送成功后更新 `LastPublishedVersion`。
- 推送失败不 ack，下一轮重试同一 source timestamp。

长周期指标：

- Valid 且未到 `ExpireAt` 时，每个发布周期都进入发布。
- 每轮使用统一的 `cycleTimestamp`。
- 推送成功后更新 `LastPublishedTimestamp`。

### 11.8 测试

- 新 clock 更新状态。
- 相同 clock/ns 不续期。
- 旧 clock 不覆盖新值。
- API 错误不增加 miss。
- 短周期超过 max age 后不发布。
- 长周期有效期内每周期可发布。
- 长周期到期后不发布。
- metadata version 变化时旧状态失效。
- 256 shard 并发读写通过 `go test -race`。

### 11.9 验收条件

- 不再依赖整体 `SetSamples` 更新时间判断过期。
- 每个 item 能独立更新和失效。
- 推送失败不会错误 ack 短周期新值。

## 12. 阶段四：采集分组和调度器

### 12.1 目的

按 Zabbix delay 调度采集，取代固定周期整轮 Collect。

### 12.2 Group 构建

分组键：

```text
hostID + valueType + normalizedDelay
```

构建时：

- 按配置过滤 host 和 item。
- 只加入 numeric float/unsigned。
- 每 group 的 itemid 使用稳定排序，保证拆批结果稳定。
- 按 `history_batch_size` 生成 batch。

### 12.3 Scheduler 实现

新增：

- `internal/scheduler/group.go`
- `internal/scheduler/scheduler.go`
- `internal/scheduler/worker_pool.go`

调度器使用：

- 一个最小堆保存 group `NextRunAt`。
- 一个固定 worker pool。
- 一个有界任务 channel。
- group `InFlight` 防止重复执行。

### 12.4 调度周期

- `delay <= 1m`：每分钟调度一次。
- `delay > 1m`：按 normalized delay 调度。
- `NextRunAt` 从上次计划时间推进，避免任务执行耗时造成持续漂移。
- 某轮执行超时后按退避重试，但不能形成无界任务积压。

### 12.5 错峰

```go
offset := hash(groupID) % spreadWindow
```

- 短周期 group 分散到一分钟内。
- 长周期 group 在自身 delay 窗口内分散。
- 启动 bootstrap 使用单独限速，避免第一次同时查询全部 group。

### 12.6 查询窗口

每个 batch 保存：

```go
type BatchWatermark struct {
    LastQueryTill time.Time
}
```

查询：

```text
time_from = LastQueryTill - overlap
time_till = 本轮固定 queryTill
```

- 只有完整成功或成功空结果才能推进 `LastQueryTill`。
- API 错误和截断失败不能推进 watermark。
- overlap 默认 `max(60s, delay*20%)`，同时设置合理上限。

### 12.7 host 失败隔离

- 单 host 慢查询只占用有限 worker。
- 可以增加 host 级并发上限，防止单 host 的多个 delay group 占满全部 worker。
- 连续失败 group 使用指数退避，但仍受 Value State 硬 TTL 约束。

### 12.8 测试

- 相同 delay 分组正确。
- value type 不混批。
- group 不会同时执行两次。
- metadata diff 能新增、删除和重建 group。
- watermark 只在完整结果时推进。
- 错峰结果稳定。
- 慢 group 不阻塞其他 group。
- context cancel 后 scheduler 和 worker 能退出。

### 12.9 验收条件

- 长周期指标不再每分钟请求 history。
- 所有 group 的调度延迟可观测。
- 调度任务数、worker 数和 channel 容量有固定上限。

## 13. 阶段五：过期时间轮和缓存清理

### 13.1 目的

取消百万级 Value Cache 的周期性全量扫描。

### 13.2 第一版实现：分钟过期桶

新增 `internal/cache/expiry_wheel.go`：

```go
type ExpiryEntry struct {
    ItemID      string
    ValueVersion uint64
    ExpireAt    time.Time
}

type ExpiryWheel struct {
    buckets map[int64][]ExpiryEntry
}
```

`bucketKey = ExpireAt.Unix() / 60`。

更新 Value State 时把新版本加入对应 bucket。旧 bucket entry 不立即删除，处理时通过 `ValueVersion` 懒惰失效。

### 13.3 清理流程

每分钟：

1. 取出当前和已经错过的 bucket。
2. 按 shard 对 entry 分组。
3. 加锁读取当前 Value State。
4. version 不同则跳过旧 entry。
5. `ExpireAt <= now` 时删除 Value State。
6. 记录删除原因和数量。

删除后不发送 stale marker，下一轮发布自然不再包含该 series。

### 13.4 时间跳跃

- 进程暂停或系统时间跳跃后，要补处理所有 `lastProcessedBucket` 到当前 bucket。
- 单轮补处理数量设置上限，避免长暂停后一次阻塞过久。
- 未处理 bucket 留到下一 tick，记录 cleanup lag。

### 13.5 升级条件

分钟桶产生的旧 version entry 太多时，再升级为支持 O(1) 移动节点的分层时间轮。第一版先通过压测确认旧 entry 的内存是否可接受。

### 13.6 测试

- 当前版本按时过期。
- 旧 version entry 不删除新值。
- metadata 主动删除后旧 expiry entry 无影响。
- 时间跳跃后补处理。
- 单桶百万 entry 的耗时和内存 benchmark。
- 清理和 ApplyBatch 并发通过 race test。

### 13.7 验收条件

- 清理不遍历所有 Value State。
- 清理耗时主要与当前到期 entry 数相关。
- 清理过程不会长时间阻塞正常采集和推送。

## 14. 阶段六：单端点流式 Publisher

### 14.1 目的

替换“先构造全部 TimeSeries、再分批”的推送模式，降低百万级内存峰值并平滑流量。

### 14.2 文件

- `internal/promwrap/publisher.go`
- `internal/promwrap/publish_slot.go`
- `internal/promwrap/batch_encoder.go`
- `internal/promwrap/push_worker.go`

### 14.3 Publish Slot

配置 `spread_slots`，默认 12：

```text
slot = seriesFingerprint % spreadSlots
```

每 5 秒处理一个 slot，每个 series 每分钟进入一次发布周期。

metadata 创建时预计算 slot，避免每次推送重新 hash 长 labels。

### 14.4 PublishPage

Publisher 不获取全部缓存快照，而是分页读取：

```go
type PublishPage struct {
    Series []PublishSeries
    Next   PublishCursor
    Done   bool
}
```

- 每页 2,000～10,000 series。
- Value Cache 只在短锁内复制必要的不可变引用和值。
- protobuf 构造、压缩和发送全部在锁外。
- 当前页发送完成或进入有界队列后再读取下一页。

### 14.5 Batch 边界

同时支持：

- `max_samples_per_send`。
- `max_batch_bytes`。

字节上限可以在编码前使用估算值，超过后结束当前 batch；压缩后记录实际大小供调参。

### 14.6 短周期发布时间

- 使用 Zabbix `SourceTimestamp + SourceNS`。
- 同一个 `ValueVersion` 只在成功 ack 前重试。
- 成功后更新 `LastPublishedVersion`。

### 14.7 长周期发布时间

- 每个 slot 周期生成固定 `cycleTimestamp`。
- 当前只有一个 endpoint；同一 slot 周期内的所有批次使用同一个周期时间基准。
- 有效缓存每分钟重新生成当前时间样本。
- `cycleTimestamp` 必须不小于该 series 上次发布时间。

### 14.8 队列和背压

- 单 endpoint 一个有界 queue。
- 固定 push worker 数。
- queue 满时不能继续无限读取 Value Cache。
- 短周期未 ack 新版本必须保留重试资格。
- 长周期普通补点积压时，可以丢弃已经过时的旧 cycle，只保留最新 cycle。
- 所有丢弃或合并必须计数。

### 14.9 重试

- HTTP 连接错误、5xx、429 按有界退避重试。
- 4xx 配置或数据错误不无限重试，记录响应体摘要。
- 重试复用原 protobuf 数据或原逻辑时间戳。
- 到达最大重试次数后：
  - 短周期不 ack ValueVersion，下一周期可重新发送。
  - 长周期等待下一轮当前值补点。

### 14.10 测试

- 每个 series 稳定进入同一 slot。
- 分页不会漏项或重复游标。
- 最大 samples 和最大 bytes 生效。
- 短周期成功后 ack，失败不 ack。
- 长周期每分钟生成新 cycle timestamp。
- queue 满时内存不继续增长。
- 慢 Remote Write 不阻塞 history scheduler。
- 批次重试保持时间戳一致。

### 14.11 验收条件

- 不再构造全量 `[]prompb.TimeSeries`。
- 推送内存峰值受 batch 和 queue 容量约束。
- 百万 series 在一分钟内平滑分布，而不是整分钟边界集中发送。

## 15. 阶段七：主流程集成

### 15.1 main 生命周期

修改 `cmd/server/main.go`，启动顺序：

```text
加载配置
    ↓
登录 Zabbix / 启动 token manager
    ↓
初始化 Metadata Cache
    ↓
完成首次 metadata 快照
    ↓
初始化 Value Cache / Expiry Wheel
    ↓
启动 Collection Scheduler
    ↓
启动 Metadata Refresher
    ↓
启动 Expiry Worker
    ↓
启动 Publisher
    ↓
启动 HTTP /metrics、health、ready
```

### 15.2 readiness

`/ready` 不只检查 token，还应检查：

- 是否存在一次成功 metadata 快照。
- scheduler 是否已启动。
- Value Cache 可以为空，不能以“没有指标值”判断未就绪。
- Publisher 未启用时不检查 Remote Write。

### 15.3 shutdown

停止顺序：

1. 停止接收新的采集任务。
2. 取消 history 请求。
3. 停止 metadata refresher。
4. 停止 publisher 新周期。
5. 等待已发送 batch 在限定时间内结束。
6. 停止 expiry worker。
7. 停止 token manager 和 HTTP server。

所有 goroutine 必须受 context 或显式 stop 控制。

### 15.4 Pull 模式

Prometheus Pull exporter 也从新的 Value Cache 读取：

- 长周期指标是否使用当前 scrape 时间，由统一 PublishPolicy 决定。
- Pull 和 Push 不能各自实现不同的过期规则。
- `/metrics` 不触发 Zabbix 采集。

### 15.5 验收条件

- 新 pipeline 能独立启动、运行和关闭。
- 采集、清理和推送 goroutine 无泄漏。
- Pull/Push 使用同一份有效状态判断。

## 16. 配置迁移计划

建议新增配置：

```yaml
collector:
  pipeline: legacy                 # legacy / scheduled
  workers: 32
  history_query_concurrency: 4
  history_batch_size: 50
  history_query_timeout: 30s
  history_max_limit: 2000
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
    streaming_enabled: false
    spread_slots: 12
    max_samples_per_send: 5000
    max_batch_bytes: 4MB
    workers: 4
    queue_capacity: 100
```

迁移规则：

- 未配置新字段时保持 legacy 行为，便于版本回滚。
- 测试环境显式开启 `pipeline: scheduled`。
- 新 pipeline 稳定后再把默认值切换为 scheduled。
- 最终删除 legacy 时另行发布不兼容变更说明。

## 17. 可观测性实施计划

### 17.1 Metadata

```text
zabbix_exporter_metadata_refresh_total{status}
zabbix_exporter_metadata_refresh_duration_seconds
zabbix_exporter_metadata_age_seconds
zabbix_exporter_metadata_hosts
zabbix_exporter_metadata_items
```

### 17.2 Scheduler 和 history

```text
zabbix_exporter_collection_groups
zabbix_exporter_scheduler_queue_entries
zabbix_exporter_scheduler_lag_seconds
zabbix_exporter_group_runs_total{status,delay_bucket}
zabbix_exporter_history_batches_total{status}
zabbix_exporter_history_response_records
zabbix_exporter_history_limit_hits_total
zabbix_exporter_history_batch_duration_seconds
```

### 17.3 Cache

```text
zabbix_exporter_value_cache_entries
zabbix_exporter_value_cache_updates_total
zabbix_exporter_value_cache_misses_total
zabbix_exporter_value_cache_expired_total{reason}
zabbix_exporter_expiry_bucket_entries
zabbix_exporter_expiry_lag_seconds
```

### 17.4 Push

```text
zabbix_exporter_push_queue_entries
zabbix_exporter_push_batches_total{status}
zabbix_exporter_push_batch_series
zabbix_exporter_push_batch_bytes
zabbix_exporter_push_slot_duration_seconds
zabbix_exporter_push_coalesced_cycles_total
```

内部指标不使用 itemid 作为 label，避免内部监控指标基数随业务指标量线性增长。

## 18. 灰度和切换计划

### 18.1 第一步：测试环境完整验证

- 使用 mock Zabbix 验证所有状态。
- 接入测试 Zabbix 验证 metadata 和 history 行为。
- Remote Write 指向测试接收端。
- 验证短周期最新点和长周期补点。

### 18.2 第二步：生产影子运行

在同一个进程内开启 scheduled pipeline，但不执行 Remote Write：

- 新 pipeline 正常刷新 metadata、调度 history 和更新 Value Cache。
- legacy pipeline 继续承担正式推送。
- 每分钟比较两边：
  - 有效 series 数量。
  - 按 metric/host/delay bucket 聚合的数量差异。
  - source timestamp 年龄分布。
  - history 请求数和响应耗时。
- 不逐 item 输出差异日志，差异明细使用有采样上限的 debug 工具。

### 18.3 第三步：单进程切换推送

- 保持只有一个正式 Publisher。
- 在配置 reload 或重启时关闭 legacy 推送，开启新 Publisher。
- 切换前确保新 Metadata Cache 已有完整快照。
- 切换时记录统一的切换时间，便于核对 Prometheus 数据。

### 18.4 第四步：观察期

重点观察：

- history error、limit hit 和 scheduler lag。
- Value Cache entry 数量和 expired 原因。
- Remote Write queue、失败率和请求体大小。
- CPU、内存、GC 和 goroutine。
- Prometheus 端 series 数、数据间隔和告警行为。

### 18.5 第五步：删除 legacy

观察期通过后：

- 删除旧 `runCollectionLoop`。
- 删除旧 `MetricCache.SetSamples` 热路径。
- 删除旧 transformer 时间戳刷新逻辑。
- 删除旧全量 `convertToTimeSeries` 推送路径。
- 清理 legacy 配置和测试。

## 19. 回滚计划

### 19.1 可回滚窗口

legacy 代码保留到新 pipeline 生产观察期结束。回滚只允许一个 Publisher 处于启用状态。

### 19.2 回滚步骤

1. 停止新 Publisher。
2. 停止 scheduled scheduler 和 expiry worker。
3. 配置切回 `collector.pipeline: legacy`。
4. 重启进程，恢复 legacy collection/push。
5. 保留新 pipeline 指标和日志用于问题分析。

### 19.3 回滚触发条件

- history 请求错误率持续高于现网基线。
- scheduler lag 持续增长。
- Remote Write queue 持续满载。
- Prometheus series 数量出现无法解释的大幅下降。
- 大面积短周期指标没有新点。
- CPU、内存或 GC 超过运行预算且不能通过调参缓解。
- 出现同 series 时间戳乱序或重复推送风暴。

## 20. 性能压测计划

### 20.1 数据规模

分级测试：

- 10 万 item。
- 50 万 item。
- 100 万 item。

delay 分布至少包含：

```text
30s：20%
60s：40%
5m：25%
30m：10%
1h：5%
```

同时覆盖不同 host 数量和每 host item 数量。

### 20.2 Zabbix mock 行为

- 正常每 item 单点。
- 单 item 窗口内多点。
- 部分 item 无数据。
- 单批达到 limit。
- 慢响应。
- 连接错误、5xx、无效 JSON。
- metadata 部分批次失败。

### 20.3 测量项

- history QPS、P50/P95/P99 响应时间。
- 每批 JSON 大小和记录数。
- reducer CPU 和分配次数。
- Value Cache 总内存和每 item 估算内存。
- shard 锁等待。
- expiry 每 tick 耗时和 backlog。
- Remote Write 每秒 series、批次大小、压缩率和失败率。
- Heap、GC、goroutine、CPU 和网络。

### 20.4 通过标准

- 所有队列在稳态下不持续增长。
- scheduler lag 在目标 delay 容忍范围内，不随时间累计。
- 缓存清理耗时不随总 item 数做固定全量线性增长。
- 推送峰值内存受 batch 和 queue 配置约束。
- 运行 24 小时无持续内存增长和 goroutine 泄漏。
- 100 万指标下 CPU、内存和网络不超过部署预算；具体预算在压测前由运行环境确定。

## 21. 测试矩阵

| 类别 | 场景 | 预期 |
|---|---|---|
| history | 一个 item 多个点 | 只保留最大 clock/ns |
| history | 多 item、多点 | 每 item 只保留最新点 |
| history | 返回达到 limit | 拆批，不记缺失 miss |
| history | API 错误 | 保留缓存，不记 miss |
| history | 成功空结果 | 对完整批次 item 记 miss |
| cache | 相同 clock 重复返回 | 不续期、不增加版本 |
| cache | 旧 clock 后到 | 不覆盖新值 |
| short | 新值未推送 | 使用 source timestamp 推送 |
| short | 推送失败 | 不 ack，允许重试 |
| short | 值过期 | 停止发布 |
| long | 有效缓存 | 每分钟使用 cycle time 补点 |
| long | 超过 ExpireAt | 停止发布 |
| metadata | 部分刷新失败 | 保留旧快照 |
| metadata | 完整刷新删除 item | 删除 group 和 value |
| scheduler | group 执行缓慢 | 不重复入队 |
| expiry | 旧 version 到期 | 不删除新 state |
| push | queue 满 | 有界背压、内存不增长 |
| lifecycle | context cancel | goroutine 全部退出 |

## 22. 实施任务依赖

```text
阶段零：基线和测试
    ↓
阶段一：history 语义修复
    ↓
阶段二：Metadata Cache
    ↓
阶段三：Value Cache
    ↓
阶段四：Scheduler
    ↓
阶段五：Expiry Wheel
    ↓
阶段六：Streaming Publisher
    ↓
阶段七：主流程集成
    ↓
影子运行 → 切换 → 观察 → 删除 legacy
```

阶段五和阶段六在 Value Cache 接口稳定后可以并行开发，但最终集成前必须完成并发和性能测试。

## 23. 每阶段提交建议

为降低 review 和回滚成本，建议拆成以下独立提交或 PR：

1. 测试基础设施和旧值复现。
2. history batch result 和 latest reducer。
3. 移除 history 未命中继承旧值。
4. metadata types/cache/refresher。
5. metadata reconciler 和 group builder。
6. sharded Value Cache 和状态机。
7. scheduler 和 worker pool。
8. history watermark、limit 拆批和错峰。
9. expiry wheel。
10. streaming publisher 和 publish slots。
11. main 生命周期、配置和 readiness 集成。
12. shadow 对比、指标和压测工具。
13. 默认切换 scheduled pipeline。
14. 删除 legacy 代码。

## 24. 完成定义

只有同时满足以下条件，改造才视为完成：

- 指标值热路径没有使用 `item.get`。
- history 多点能按 item 正确选取最新 clock/ns。
- history 空结果不会继承旧值并刷新时间戳。
- history 错误和 limit 截断不会误删缓存。
- 长短周期规则均有自动化测试。
- metadata 与 value 已分离。
- Value Cache 已分片，清理不做全量扫描。
- Remote Write 已流式分批并在一分钟内错峰。
- 所有队列、worker 和重试均有界。
- 单 endpoint 推送失败不会阻塞采集。
- 10 万、50 万、100 万三级压测完成。
- 生产影子对比和观察期通过。
- legacy 删除前仍具备明确回滚路径。
