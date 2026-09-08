# Zabbix Exporter

[English](README.md) | 简体中文

[![CI](https://github.com/zhaeng/zabbix-exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/zhaeng/zabbix-exporter/actions/workflows/ci.yml)
[![CodeQL](https://github.com/zhaeng/zabbix-exporter/actions/workflows/codeql.yml/badge.svg)](https://github.com/zhaeng/zabbix-exporter/actions/workflows/codeql.yml)
[![License](https://img.shields.io/github/license/zhaeng/zabbix-exporter)](LICENSE)

将 Zabbix 采集的数值型监控数据转换为 Prometheus 指标。支持 Prometheus Pull 和 Remote Write Push，两种模式共享同一份内存缓存，抓取 `/metrics` 不会额外请求 Zabbix API。

> 本项目是社区项目，与 Zabbix LLC、Prometheus Authors 或 Grafana Labs 无隶属或官方关联。相关名称和商标归各自权利人所有。

## 功能

- 使用 Zabbix API key，或用户名和密码认证
- 导出 numeric float 和 numeric unsigned 类型的 Zabbix item
- 按主机组、主机 IP、item key 正则和 value type 过滤
- 提供 `/metrics`、`/internal/metrics`、`/health` 和 `/ready`
- 可选 Prometheus Remote Write，支持有界队列、分页、错峰和重试
- metadata 刷新、分组调度、分片缓存和过期清理
- 提供 Grafana dashboard 与 Kubernetes 示例

## 要求

- Go 1.25 或更高版本；推荐使用 `go.mod` 指定的安全补丁工具链
- 可访问的 Zabbix JSON-RPC API
- API key，或具备读取 host、host group、item 和 history 权限的 Zabbix 用户

## 快速开始

```bash
git clone https://github.com/zhaeng/zabbix-exporter.git
cd zabbix-exporter
cp config.example.yaml config.yaml
export ZABBIX_API_KEY='your-zabbix-api-key'
go run ./cmd/server -c config.yaml
```

检查服务：

```bash
curl http://localhost:9110/health
curl http://localhost:9110/ready
curl http://localhost:9110/metrics
```

`config.yaml` 已被 Git 忽略。配置文件支持 `${ENVIRONMENT_VARIABLE}` 展开，请使用环境变量或密钥管理系统提供凭据。

## Docker

```bash
docker build -t zabbix-exporter:local .
docker run --rm -p 9110:9110 \
  -e ZABBIX_API_KEY \
  -v "$PWD/config.yaml:/etc/zabbix-exporter/config.yaml:ro" \
  zabbix-exporter:local
```

## Prometheus

```yaml
scrape_configs:
  - job_name: zabbix-exporter
    static_configs:
      - targets: ["zabbix-exporter:9110"]

  - job_name: zabbix-exporter-self
    metrics_path: /internal/metrics
    static_configs:
      - targets: ["zabbix-exporter:9110"]
```

- `/metrics`：Zabbix 业务指标及 exporter 自监控指标
- `/internal/metrics`：仅 exporter、Go 和 process 自监控指标
- `/health`：进程存活检查
- `/ready`：认证、metadata 和 scheduler 就绪检查

## 配置

完整配置和默认建议见 [配置参考](docs/configuration_zh-CN.md) 和 [config.example.yaml](config.example.yaml)。启用 Push 时必须且只能配置一个 Remote Write endpoint：

```yaml
prometheus:
  push:
    enabled: true
    remote_write:
      endpoints:
        - url: "https://prometheus.example.com/api/v1/write"
          timeout: 30s
          max_samples_per_send: 2000
```

API key 优先于用户名和密码。仅在无法导入内部 CA 的受控环境中使用 `tls_skip_verify: true`。

## 构建与验证

```bash
make fmt-check
make vet
make test
make test-race
make build
```

## Kubernetes 和 Grafana

- Kubernetes 示例：[deployments/kubernetes](deployments/kubernetes/README_zh-CN.md)
- Grafana dashboard：[deployments/zabbix-exporter-grafana-dashboard.json](deployments/zabbix-exporter-grafana-dashboard.json)

部署前请修改示例镜像地址，并创建 `zabbix-exporter-secrets`。不要将真实 Secret 提交到仓库。

## 文档

- [架构与数据流](docs/architecture_zh-CN.md)
- [配置参考](docs/configuration_zh-CN.md)
- [自监控指标](docs/metrics_zh-CN.md)
- [运维与排障](docs/operations_zh-CN.md)

## 安全说明

指标可能包含主机名、IP 和业务标签。默认 HTTP 端点没有认证，请通过私有网络、NetworkPolicy、防火墙或带认证的反向代理限制访问。安全问题请按照 [SECURITY.md](SECURITY_zh-CN.md) 私下报告。

## 参与贡献

欢迎 Issue 和 Pull Request。开始前请阅读 [CONTRIBUTING.md](CONTRIBUTING_zh-CN.md) 和 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT_zh-CN.md)。

## 许可证

本项目采用 [Apache License 2.0](LICENSE)。
