# History 自适应调参实施计划

> 状态：规划完成，待实施  
> 编制日期：2026-09-03  
> 代码基线：`af6d109` 及工作区内尚未提交的 QueryTill 修复  
> 任务入口：[A00-A06 任务清单](implementation_tasks/README.md#history-自适应调参任务)

## 1. 背景与判断

当前生产现象同时包含调度拥塞和 History API 结果差异：scheduler queue 长时间满载、worker/inflight 接近上限，但 API 延迟、超时、limit 命中和返回完整性并不总是同步恶化。仅按队列长度统一放大并发或批大小，可能把 Zabbix API 压垮；仅按 API 延迟降速，又可能让健康 API 无法消化积压。

本方案把决策拆成两层：

1. **拥塞门控层**判断当前是否需要调参。
2. **API 决策层**根据请求结果、耗时和完整性，决定调整哪个参数以及调整方向。

安全例外：即使没有 scheduler 拥塞，只要 API 已出现持续超时、错误或源端异常，也允许保护性降档；不允许无拥塞时主动升档。

## 2. 目标

- 在持续拥塞时自动寻找当前 Zabbix API 可承受的 History 吞吐。
- API 健康时优先增加单请求有效载荷，必要时再增加并发。
- limit 命中、结果可能截断、超时或错误上升时自动选择正确的降档参数。
- 保证动态调参不破坏查询窗口、稳定 batch ID、watermark、miss 和 ValueCache 语义。
- 支持 `observe` 影子决策、`enforce` 生效和一键回退到固定配置。
- 用低基数指标和结构化日志解释每次决策。

## 3. 非目标与禁止自动调整项

首期不自动调整以下参数：

- `scheduler_queue_capacity`：扩大队列只能容纳更多等待，不能提高服务能力。
- scheduler worker 数：worker 应预留到不低于允许的最大 History 并发，运行时只调整请求准入。
- `history_query_timeout`：动态延长会掩盖 API 退化，并扩大尾延迟。
- query overlap、bootstrap window、缓存 TTL：它们属于正确性/业务语义，不属于吞吐旋钮。
- metadata 刷新、Remote Write 批大小和推送并发：与 History 控制器分开治理。

首期也不以“队列清零”为目标；在输入速率持续大于可安全处理能力时，应保持 Zabbix 稳定并告警容量不足。

## 4. 不可破坏的正确性约束

1. `queryTill` 必须在 worker 真正开始执行时生成，排队时间不能吞掉查询窗口。
2. 一个逻辑 batch 的所有物理分片和递归拆分共享同一个 `queryTill`。
3. 只有全部物理分片完整成功后，逻辑 batch 才推进 watermark、提交 miss。
4. timeout、API error、decode error、取消、未解决的 limit hit 均不推进 watermark，也不提交整批 miss。
5. 已完整返回的值可以幂等 upsert；后续依靠 `clock/ns` 和 overlap 去重补齐。
6. 动态物理批大小不得改变逻辑 batch ID；否则会丢失既有 watermark 并制造重复回查。
7. 缩小并发不取消已执行请求，只阻止新的 acquire，直到 active 不高于新上限。
8. 控制器异常、panic 或状态缺失时回到最后有效值或静态配置，不得停止采集。

## 5. 术语与控制旋钮

- **逻辑 batch**：由稳定分组规则生成，是 batch ID、watermark 和 miss 提交的边界。
- **物理分片**：一次实际 `history.get` 携带的 item 子集，大小可动态改变。
- **History 并发**：全局允许同时执行的 `history.get` 数量。
- **控制窗口**：控制器聚合信号并作出一次候选决策的周期，默认 1 分钟。
- **冷却期**：一次生效调整后禁止再次升降档的时间，默认 3 分钟。

首期只允许调整两个旋钮：

| 旋钮 | 初始值 | 最小值 | 最大值 | 单次调整 |
|---|---:|---:|---:|---|
| History 并发 | 现有 `history_query_concurrency`，建议 8 | 4 | 16 | 升 1；降至当前值的 75% |
| 物理 batch item 数 | 现有 `history_batch_size`，建议 100 | 50 | 200 | 升 25；降至当前值的 50% |

每个控制窗口最多修改一个旋钮，所有边界均可配置。

## 6. 信号模型

### 6.1 拥塞信号：决定是否需要调参

控制器从内部事件快照读取信号，不抓取自身 `/metrics`：

- queue occupancy：`scheduler_queue_entries / scheduler_queue_capacity`。
- `queue_full` 增量和占 group run 的比例。
- scheduler lag：任务计划时间到 worker 开始时间的差值。
- History queue wait：进入请求准入队列到获得并发令牌的时间。
- ValueCache freshness/entries 的持续下降，仅作为辅助证据。

建议初始状态定义：

- `congested`：queue occupancy ≥ 80%，或 scheduler lag P95 ≥ 30 秒，连续 2 个窗口成立。
- `recovering`：曾拥塞，但当前低于阈值且尚未连续 5 个窗口恢复。
- `clear`：连续 5 个窗口低于高水位、无显著 `queue_full`，且 lag 恢复。

阈值使用滞回，避免在边界上下振荡。具体阈值必须先在 `observe` 模式用真实分布校准。

### 6.2 API 信号：决定调什么

按 JSON-RPC method 聚合，控制器只消费 `history.get`：

- 请求数、成功数、timeout、transport error、HTTP/API error、decode error。
- 请求耗时 P50/P95/P99。
- requested item 数、returned record 数。
- limit hit、递归 split、最大 split depth、未解决截断。
- 完整逻辑 batch、失败逻辑 batch和成功空 batch。
- returned/limit fill ratio，用于判断单请求是否接近返回上限。

建议初始分级：

- `healthy`：错误率和 timeout 率均低于 0.5%，P95 < 2 秒，无未解决截断。
- `latency_overload`：P95 > 5 秒，或 timeout/错误率 > 2%。
- `limit_pressure`：出现 limit hit、split 或 possibly-truncated。
- `source_unhealthy`：全局 empty 比例突增，同时 freshness/覆盖率明显下降，或认证/API 系统性失败。

### 6.3 数据完整性

一个逻辑 batch 仅在以下条件全部成立时记为 complete：

- 响应是有效 JSON-RPC，且无 timeout/API/decode error。
- 未命中 limit；或者所有递归/物理分片都已完整成功。
- 查询期间使用的 metadata 版本有效，结果可以映射到逻辑 batch。

空结果可以是完整结果，不能单独判为故障。只有空结果全局突增并伴随 freshness/coverage 下跌时，才分类为 source unhealthy。

## 7. 决策状态机

控制优先级从高到低如下：

| 拥塞状态 | API/结果状态 | 动作 | 原因 |
|---|---|---|---|
| 任意 | source unhealthy | 冻结升档；必要时降并发；告警 | 源端故障不是容量不足 |
| 任意 | limit pressure / incomplete | 降低物理 batch | 减少单次结果规模，保护完整性 |
| 任意 | 高延迟/超时且随大 batch 相关 | 降低物理 batch | 降低单请求负担 |
| 任意 | 高延迟/超时与请求大小无明显相关 | 降低并发 | 降低 Zabbix 并行压力 |
| congested | API healthy，batch 未到上限 | 增加物理 batch | 优先摊薄请求固定开销 |
| congested | API healthy，batch 已到上限 | 增加并发 | 用更多并行吞吐消化积压 |
| clear/recovering | API healthy | 保持 | 无拥塞不主动扩容 |

补充规则：

- 启动后 warmup 5 分钟，仅观测不调整。
- 拥塞需连续 2 个窗口才允许升档，恢复需连续 5 个窗口才退出拥塞状态。
- 调整后进入 3 分钟 cooldown；保护性降档可绕过升档冷却，但同一窗口仍只动一个旋钮。
- 信号样本不足、分母为零或快照过期时返回 `hold/insufficient_data`。
- 达到上下界时不重复写入，仅增加带有有界 `reason` 标签的决策计数。
- 决策函数必须是纯函数：相同快照和当前参数得到相同结果，便于回放与测试。

## 8. 运行时实现设计

### 8.1 内部信号快照

新增低开销事件聚合层，记录固定时间桶，不保存 item ID、host ID、URL 或错误文本等高基数值。Prometheus 指标和控制器共享事件源，但控制器不依赖 Prometheus registry 或 PromQL。

建议接口：

```go
type AdaptiveSnapshot struct {
    WindowStart, WindowEnd time.Time
    QueueOccupancy         float64
    QueueFullRatio         float64
    SchedulerLagP95        time.Duration
    QueueWaitP95           time.Duration
    History                HistoryWindow
    Completeness           CompletenessWindow
}

type Decision struct {
    Action string // hold, set_batch, set_concurrency
    Value  int
    Reason string // bounded enum
}
```

### 8.2 可动态调整的并发限制器

现有固定容量 channel 无法安全缩容。替换为 resizable limiter：

- `Acquire(ctx)` 在 active < limit 时通过，否则等待并响应取消。
- `Release()` 唤醒等待者。
- `SetLimit(n)` 原子更新目标；缩容时不终止 active 请求。
- 公开 current limit、active 和 waiter 数快照。
- pipeline 关闭时解除所有等待并防止 goroutine 泄漏。

scheduler worker 数使用静态上限并保持不低于并发最大值；真正的 API 并行度由 limiter 控制。

### 8.3 稳定逻辑 batch 与动态物理分片

GroupBuilder 继续按稳定上限生成逻辑 batch，运行时 collector 再按当前物理 batch size 切片。一个逻辑 batch 的执行过程：

1. worker 开始时生成一次 `queryTill`。
2. 根据逻辑 batch watermark 生成固定查询窗口。
3. 按当前物理 batch size 稳定切片，所有切片复用该窗口。
4. 每个切片仍可因 API limit 递归二分。
5. 聚合所有叶子结果；全部 complete 才统一推进 watermark 和提交 miss。

控制轮中 batch size 改变只影响尚未开始的逻辑 batch；执行中的 batch 使用启动时快照，防止同轮边界漂移。

## 9. 配置草案

默认关闭自动生效，先以 `observe` 模式发布。现有静态字段作为初始值和关闭后的 fallback。

```yaml
collector:
  workers: 32
  history_query_concurrency: 8
  history_batch_size: 100
  history_logical_batch_size: 200

  adaptive_history:
    enabled: false
    mode: observe # observe | enforce
    interval: 1m
    warmup: 5m
    cooldown: 3m
    congestion_consecutive_periods: 2
    recovery_consecutive_periods: 5
    queue_high_watermark: 0.80
    scheduler_lag_threshold: 30s
    concurrency:
      min: 4
      max: 16
      increase_step: 1
      decrease_ratio: 0.75
    physical_batch:
      min: 50
      max: 200
      increase_step: 25
      decrease_ratio: 0.50
    api:
      healthy_p95: 2s
      overload_p95: 5s
      healthy_error_ratio: 0.005
      overload_error_ratio: 0.02
```

校验要求：min ≤ 静态初始值 ≤ max、比例在 `(0,1)`、所有 duration 为正、workers ≥ concurrency max。配置非法时启动失败并给出具体字段，不静默修正。

## 10. 可观测性

建议新增：

```text
zabbix_exporter_adaptive_history_info{mode}
zabbix_exporter_adaptive_history_state{state}
zabbix_exporter_adaptive_history_concurrency
zabbix_exporter_adaptive_history_batch_size
zabbix_exporter_adaptive_decisions_total{action,reason}
zabbix_exporter_history_root_batches_total{status}
zabbix_exporter_history_splits_total{reason}
zabbix_exporter_history_queue_wait_seconds
zabbix_exporter_history_requested_items
zabbix_exporter_history_returned_records
zabbix_exporter_history_item_coverage_ratio
```

`action`、`reason`、`status`、`state` 必须是代码内固定枚举。不得加入 item、host、batch ID、错误字符串或 URL 标签。

每次候选和生效决策记录结构化日志：mode、state、action、reason、old/new、queue occupancy、lag P95、API P95、error ratio、limit hit ratio、completeness ratio。API key 不得进入日志。

Grafana 应展示：当前控制状态和参数、拥塞信号、API 延迟/错误/吞吐、完整逻辑 batch 比例、limit/split、缓存 freshness、Remote Write 推送健康。控制器指标仅从 `/internal/metrics` 拉取，避免业务序列重复。

## 11. 发布与回滚

1. **基线**：先合入并验证 QueryTill 修复，记录固定参数下至少 30 分钟基线。
2. **observe**：控制器计算但不执行，持续 24 小时；回放每次决策并人工核对。
3. **enforce 限制范围**：先只允许降档，再开放 batch 升档，最后开放并发升档。
4. **生产门禁**：每阶段至少覆盖一个高峰周期；完整率不得下降，timeout/error 不得持续上升。
5. **回滚**：设置 `enabled: false` 并重启，恢复静态 `history_query_concurrency` 和 `history_batch_size`；无需迁移 watermark。

禁止控制器自动部署、自动修改配置文件或绕过生产授权。

## 12. 验证与验收

### 12.1 自动化测试

- 滚动窗口边界、空窗口、时钟跳变和并发事件 race。
- limiter 扩容、缩容、取消、关闭、公平性和 goroutine 泄漏。
- 动态物理切片不改变逻辑 batch ID/watermark；任一分片失败不提交 miss。
- 决策矩阵表驱动测试；warmup、cooldown、滞回、上下界和单旋钮约束。
- 录制快照回放：健康拥塞升 batch、limit 降 batch、独立 timeout 降并发、源故障冻结。
- `go test ./...`、`go test -race ./...`、`go vet ./...`、`git diff --check`。

### 12.2 性能与生产验收

- 控制器和事件聚合 CPU/内存开销可忽略，不引入高基数序列。
- 在可控压测下，积压可恢复且无 watermark/miss 错误。
- observe 决策与人工判断一致，不出现连续反向振荡。
- enforce 后 scheduler lag、queue_full 下降，History complete ratio 不下降。
- API timeout/error、P95/P99 和 limit hit 保持在配置保护阈值内。
- exporter 重启、配置关闭和控制器故障均可安全回到静态参数。

## 13. 任务拆分与依赖

| ID | 任务 | 产出 | 前置依赖 |
|---|---|---|---|
| A00 | [QueryTill 正确性与自适应基线](implementation_tasks/A00-QueryTill正确性与自适应基线.md) | 正确查询窗口、基线报告 | 无 |
| A01 | [自适应信号与滚动窗口](implementation_tasks/A01-自适应信号与滚动窗口.md) | 内部事件、快照、信号指标 | A00 |
| A02 | [可动态调整 History 并发限制器](implementation_tasks/A02-可动态调整History并发限制器.md) | resizable limiter | A01 接口冻结 |
| A03 | [稳定逻辑批次与动态物理分片](implementation_tasks/A03-稳定逻辑批次与动态物理分片.md) | 动态 batch、完整性聚合 | A01 接口冻结 |
| A04 | [拥塞门控与 API 决策控制器](implementation_tasks/A04-拥塞门控与API决策控制器.md) | 纯决策引擎、状态机 | A01、A02、A03 |
| A05 | [Pipeline 集成、配置与可观测性](implementation_tasks/A05-Pipeline集成配置与可观测性.md) | observe/enforce 集成 | A04 |
| A06 | [影子运行、压测与生产门禁](implementation_tasks/A06-影子运行压测与生产门禁.md) | 验收报告、上线/回滚结论 | A05 |

```text
A00 → A01 ─┬→ A02 ─┐
           └→ A03 ─┴→ A04 → A05 → A06
```

A02 与 A03 可在 A01 的快照和执行器接口冻结后并行。A06 只产出验证结果和上线建议；切换生产仍需单独明确授权。

## 14. 实施决策记录

- 当前工作区已有未提交的 QueryTill 修复；A00 必须先独立审查、测试和提交，后续任务不得顺带重写该修改。
- 自适应参数只存在于进程内，重启从静态配置初始值开始，首期不持久化学习状态。
- 首期采用规则控制器而非 PID/机器学习，优先保证可解释、可回放和可回滚。
- 任何优化都服从数据完整性；无法证明完整时宁可重查，不推进 watermark。
