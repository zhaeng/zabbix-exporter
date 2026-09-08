# Zabbix Exporter 设计与规格文档

## 1. 项目概述

**项目名称**: zabbix-exporter
**项目类型**: Go 语言编写的指标导出服务
**核心功能**: 将 Zabbix 采集的监控数据转换为 Prometheus 指标格式，支持主动推送(Push)和被动抓取(Pull)两种模式
**目标用户**: 需要将 Zabbix 监控数据接入 Prometheus 生态的用户

### 快速开始

复制示例配置并通过环境变量提供 Zabbix API key：

```bash
cp config.example.yaml config.yaml
export ZABBIX_API_KEY='your-zabbix-api-key'
go run ./cmd/server -c config.yaml
```

默认指标地址为 `http://localhost:9110/metrics`，就绪检查地址为 `http://localhost:9110/ready`。`config.yaml` 已被 Git 忽略，请勿将真实凭据提交到仓库。

---

## 2. 核心功能设计

### 2.1 数据采集模式

#### Pull 模式 (被动抓取)
- 启动 HTTP Server，监听指定端口（默认 `:9110`）
- 实现 `/metrics` 接口，Prometheus 通过该接口拉取指标
- 实现 `/health` 健康检查接口
- 实现 `/ready` 就绪检查接口

#### Push 模式 (主动推送)
- 使用 Prometheus Remote Write 协议推送指标
- 启用 Push 时只允许配置一个 Remote Write 端点
- scheduled publisher 将 ValueCache 分页、分槽推送，默认周期为 60 秒
- Pull 与 Push 读取同一个 ValueCache；抓取 `/metrics` 不会调用 Zabbix API
- Remote Write 配置参数：
  - endpoint `timeout`: 单次远程写入超时（默认 30s）
  - `max_samples_per_send`: 每次发送最大样本数
  - `workers` / `queue_capacity`: 有界发送 worker 与队列
  - `max_batch_bytes`: 单批未压缩数据上限

### 2.2 Zabbix 认证与 Token 管理

#### Token 缓存机制
- 启动时进行 API 登录，获取初始 token
- Token 存储在内存缓存中
- Token 有效期监控（Zabbix 默认 8 小时）

#### Token 定时刷新
- 后台 goroutine 定时检查 token 状态
- 提前 5 分钟刷新 token（可配置）
- 刷新失败时记录日志并重试
- 指数退避重试策略：1s, 2s, 4s, 8s, 最大 60s

### 2.3 指标处理

#### 指标过滤
- 支持通过正则表达式过滤指标名称
- 支持白名单/黑名单两种过滤模式
- 支持按 host groups 过滤
- 支持按 item key 过滤

#### 指标转换
- Zabbix item key → Prometheus metric name（做规范化处理）
- 自动处理 Zabbix 值类型映射：
  - `Numeric (unsigned)` → `gauge` 或 `counter`
  - `Numeric (float)` → `gauge` 或 `counter`
  - `Character` → 不导出（文本类型）
  - `Log` → 不导出（日志类型）
  - `Text` → 不导出（文本类型）
- 支持配置指标类型（gauge/counter）

#### Label 配置
- 全局 labels：所有指标都附带
- Host 级 labels：按主机维度附加
- Item 级 labels：按指标维度附加
- 支持从 Zabbix 主机属性动态获取 label 值

### 2.4 配置管理

#### 配置文件 (config.yaml)
```yaml
zabbix:
  url: "http://zabbix-server/zabbix/api_jsonrpc.php"
  username: "Admin"
  password: "zabbix"
  refresh_token_interval: 300  # 提前刷新 token 秒数

prometheus:
  pull:
    enabled: true
    port: 9110
    path: "/metrics"
    internal_path: "/internal/metrics"
  push:
    enabled: false
    interval: 60
    spread_slots: 12
    page_size: 5000
    max_batch_bytes: 4194304
    workers: 4
    queue_capacity: 100
    remote_write:
      endpoints:
        - url: "http://prometheus:9090/api/v1/write"
          timeout: 30s
          max_samples_per_send: 2000

collector:
  workers: 32
  scheduler_queue_capacity: 128
  history_query_concurrency: 4
  history_batch_size: 50
  history_query_timeout: 30s
  history_max_limit: 2000
  query_overlap: 5m
  startup_spread_window: 60s
  metadata_refresh_interval: 20m
  metadata_workers: 2
  metadata_batch_hosts: 50

labels:
  global:
    env: "production"
    region: "us-east"
  host:
    cluster: "{{host.metadata.cluster}}"
  item: {}

metrics:
  filter:
    mode: "whitelist"  # whitelist 或 blacklist
    patterns:
      - ".*cpu.*"
      - ".*memory.*"
  host_groups:
    - "Linux servers"
  value_types:
    - "numeric"

cache:
  shards: 256
  expiry_tick: 1m
  expiry_max_entries_per_run: 100000
  miss_threshold: 2
```

Zabbix API key 认证可以替代 `username`/`password`；两者同时存在时优先使用 `api_key`，且不执行 `user.login` 或周期刷新：

```yaml
zabbix:
  url: "https://zabbix.example.com/api_jsonrpc.php"
  api_key: "${ZABBIX_API_KEY}"
  tls_skip_verify: false  # 仅内部自签名证书且无法导入 CA 时设为 true
```

Zabbix API 请求提供按 JSON-RPC method 区分的 `zabbix_exporter_api_requests_total`、`zabbix_exporter_api_request_duration_seconds` 和 `zabbix_exporter_api_requests_in_flight`。最近 5 分钟的平均请求时长可用以下 PromQL 计算：

```promql
rate(zabbix_exporter_api_request_duration_seconds_sum[5m])
/
rate(zabbix_exporter_api_request_duration_seconds_count[5m])
```

`/metrics` 继续同时暴露 Zabbix 业务指标和 exporter 自监控指标；`/internal/metrics` 只暴露 exporter、Go 和 process 自监控指标，可供 Prometheus 单独抓取，避免与 Remote Write 的 Zabbix 业务序列重复。

缓存覆盖可通过 `zabbix_exporter_metadata_enabled_items`、`zabbix_exporter_value_cache_valid_entries` 和 `zabbix_exporter_value_cache_fresh_entries` 判断。`valid` 表示状态仍有效，`fresh` 还要求当前时间早于值的硬过期时间；缓存统计按 expiry tick 低频扫描更新。item 变为 disabled 后不再采集新值，已有 ValueCache 状态不主动删除并在硬 TTL 到期后自然清理，因此两个缓存 gauge 在这段时间内允许包含少量 disabled item。

```promql
zabbix_exporter_value_cache_fresh_entries
/
clamp_min(zabbix_exporter_metadata_enabled_items, 1)
```

```yaml
scrape_configs:
  - job_name: zabbix-exporter-self
    metrics_path: /internal/metrics
    static_configs:
      - targets: ["zabbix-exporter:9110"]
```

scheduled pipeline 是唯一运行路径。`prometheus.push.enabled: false` 时服务以 scheduled pull-only 方式运行；设为 `true` 时必须且只能配置一个 Remote Write endpoint。旧 YAML 中残留的 `collector.pipeline`、`prometheus.push.streaming_enabled`、`cache.refresh_interval`、`collector.max_concurrent_hosts` 和 endpoint `max_shards` 会作为未知字段被忽略，不再选择运行路径，建议在配置迁移时删除。详细迁移与本地版本回滚步骤见 [C01 配置迁移与版本回滚](docs/C01-scheduled配置迁移与版本回滚.md)。

---

## 3. API 接口设计

### 3.1 HTTP Server

| 端点 | 方法 | 描述 |
|------|------|------|
| `/metrics` | GET | Zabbix 业务指标与 exporter 自监控指标 |
| `/internal/metrics` | GET | 仅 exporter、Go 和 process 自监控指标 |
| `/health` | GET | 健康检查（仅返回 200） |
| `/ready` | GET | 就绪检查（token、完整 metadata 快照与 scheduler） |
| `/config/reload` | POST | 热重载配置 |

### 3.2 内部 API

| 接口 | 描述 |
|------|------|
| Zabbix API | 与 Zabbix server 通信 |
| Prometheus Remote Write API | 推送指标到远程 |

---

## 4. 数据结构设计

### 4.1 核心数据结构

```go
// Config 应用配置
type Config struct {
    Zabbix     ZabbixConfig
    Prometheus PrometheusConfig
    Labels     LabelsConfig
    Metrics    MetricsFilterConfig
    Cache      CacheConfig
}

// ZabbixConfig Zabbix 连接配置
type ZabbixConfig struct {
    URL                  string
    Username             string
    Password             string
    RefreshTokenInterval int
}

// PrometheusConfig Prometheus 配置
type PrometheusConfig struct {
    Pull  PullConfig
    Push  PushConfig
}

// PullConfig Pull 模式配置
type PullConfig struct {
    Enabled      bool
    Port         int
    Path         string
    InternalPath string
}

// PushConfig Push 模式配置
type PushConfig struct {
    Enabled   bool
    Endpoints []string
    Interval  int
}

// LabelsConfig Label 配置
type LabelsConfig struct {
    Global map[string]string
    Host   map[string]string
    Item   map[string]string
}

// MetricsFilterConfig 指标过滤配置
type MetricsFilterConfig struct {
    Filter     FilterConfig
    HostGroups []string
    ValueTypes []string
}

// FilterConfig 过滤模式配置
type FilterConfig struct {
    Mode     string   // "whitelist" 或 "blacklist"
    Patterns []string // 正则表达式列表
}

// TokenInfo Token 信息
type TokenInfo struct {
    Token     string
    ExpiresAt time.Time
   Mu        sync.RWMutex
}
```

### 4.2 Zabbix API 数据结构

```go
// Zabbix API 响应结构
type APIResponse struct {
    JSONRPC string      `json:"jsonrpc"`
    Result  interface{} `json:"result"`
    Error   *APIError   `json:"error,omitempty"`
}

// Item Zabbix 监控项
type Item struct {
    ItemID      string  `json:"itemid"`
    HostID      string  `json:"hostid"`
    Key         string  `json:"key_"`
    Name        string  `json:"name"`
    ValueType   string  `json:"value_type"`
    LastValue   string  `json:"lastvalue,omitempty"`
    LastClock   int     `json:"lastclock,omitempty"`
}

// Host Zabbix 主机
type Host struct {
    HostID string `json:"hostid"`
    Host   string `json:"host"`
    Name   string `json:"name"`
}
```

---

## 5. 模块设计

### 5.1 模块架构

```
zabbix-exporter/
├── cmd/
│   └── server/
│       └── main.go           # 程序入口
├── internal/
│   ├── config/
│   │   └── config.go         # 配置加载
│   ├── zabbix/
│   │   ├── client.go         # Zabbix API 客户端
│   │   └── token.go          # Token 管理
│   ├── collector/
│   │   └── collector.go      # 指标采集器
│   ├── filter/
│   │   └── filter.go         # 指标过滤器
│   ├── transformer/
│   │   └── transformer.go    # 指标转换器
│   ├── cache/
│   │   └── cache.go          # 缓存管理
│   ├── prometheus/
│   │   ├── pull.go           # Pull 模式 HTTP Server
│   │   └── push.go           # Push 模式推送
│   └── metrics/
│       └── metrics.go        # Prometheus 指标定义
├── pkg/
│   └── utils/
│       └── utils.go          # 工具函数
├── config.yaml               # 配置文件
├── go.mod
├── go.sum
└── SPEC.md
```

### 5.2 模块职责

| 模块 | 职责 |
|------|------|
| config | 加载并解析 YAML 配置文件 |
| zabbix/client | 封装与 Zabbix API 的所有通信 |
| zabbix/token | 管理 Zabbix token 的获取、缓存、刷新 |
| collector | 从 Zabbix 采集监控数据 |
| filter | 根据配置规则过滤指标 |
| transformer | 将 Zabbix 数据转换为 Prometheus 指标格式 |
| cache | 缓存历史数据，支持指标去重和时间窗口 |
| prometheus/pull | 提供 HTTP Server 供 Prometheus 拉取 |
| prometheus/push | 使用 Remote Write v2 协议将指标推送到 Prometheus |

---

## 6. 功能优先级

### P0 (核心功能)
1. Zabbix API 客户端实现
2. Token 管理与自动刷新
3. Pull 模式 HTTP Server
4. 基础指标转换

### P1 (重要功能)
5. Push 模式支持
6. 指标过滤功能
7. Label 配置支持

### P2 (增强功能)
8. 配置热重载
9. 历史数据缓存
10. 健康检查/就绪检查

---

## 7. 错误处理

### 7.1 错误分类

| 错误类型 | 处理策略 |
|----------|----------|
| Zabbix 连接失败 | 记录日志，继续重试 |
| Token 刷新失败 | 指数退避重试，记录日志 |
| 指标转换失败 | 跳过该指标，记录警告 |
| Push 失败 | 缓存指标，下次重试 |

### 7.2 日志级别

- `ERROR`: 严重错误，需要告警
- `WARN`: 警告，不影响核心功能
- `INFO`: 一般信息
- `DEBUG`: 调试信息

---

## 8. 配置项说明

### 8.1 环境变量

| 变量名 | 描述 | 默认值 |
|--------|------|--------|
| `ZBX_URL` | Zabbix API 地址 | - |
| `ZBX_USERNAME` | Zabbix 用户名 | - |
| `ZBX_PASSWORD` | Zabbix 密码 | - |
| `CONFIG_PATH` | 配置文件路径 | `./config.yaml` |

### 8.2 配置参数范围

| 参数 | 最小值 | 最大值 | 默认值 |
|------|--------|--------|--------|
| pull.port | 1 | 65535 | 9110 |
| push.interval | 5 | 3600 | 30 |
| cache.history_duration | 60 | 3600 | 300 |
| refresh_token_interval | 60 | 3600 | 300 |

---

## 9. 测试策略

### 9.1 单元测试
- config 配置解析
- filter 过滤逻辑
- transformer 转换逻辑
- token 管理逻辑

### 9.2 集成测试
- Zabbix API 连接测试
- Prometheus 指标抓取测试
- Push 模式推送测试

---

## 10. 部署方式

### 10.1 Docker 部署
```dockerfile
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o zabbix-exporter ./cmd/server

FROM alpine
COPY --from=builder /app/zabbix-exporter /usr/local/bin/
COPY --from=builder /app/config.yaml /etc/zabbix-exporter/config.yaml
EXPOSE 9110
CMD ["zabbix-exporter", "-config", "/etc/zabbix-exporter/config.yaml"]
```

### 10.2 Docker Compose 部署示例
```yaml
version: '3'
services:
  zabbix-exporter:
    image: zabbix-exporter:latest
    ports:
      - "9110:9110"
    volumes:
      - ./config.yaml:/etc/zabbix-exporter/config.yaml
    environment:
      - ZBX_URL=http://zabbix-server/zabbix/api_jsonrpc.php
      - ZBX_USERNAME=Admin
      - ZBX_PASSWORD=zabbix
```

---

## 11. 安全考虑

1. **敏感信息**: 配置文件中密码等敏感信息支持环境变量引用
2. **HTTPS**: Zabbix API 建议使用 HTTPS 连接
3. **Token 安全**: Token 仅存储在内存中，不持久化

---

## 12. 性能考虑

1. **并发采集**: 支持配置并发采集数量
2. **批处理**: Push 模式支持批量推送减少网络开销
3. **缓存**: 历史数据缓存减少 Zabbix API 调用
4. **连接复用**: HTTP 客户端使用连接池
