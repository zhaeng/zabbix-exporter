# Zabbix Exporter

English | [简体中文](README.md)

[![CI](https://github.com/zhaeng/zabbix-exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/zhaeng/zabbix-exporter/actions/workflows/ci.yml)
[![CodeQL](https://github.com/zhaeng/zabbix-exporter/actions/workflows/codeql.yml/badge.svg)](https://github.com/zhaeng/zabbix-exporter/actions/workflows/codeql.yml)
[![License](https://img.shields.io/github/license/zhaeng/zabbix-exporter)](LICENSE)

Zabbix Exporter turns numeric data collected by Zabbix into Prometheus metrics. It supports both Prometheus scraping and Remote Write. Both output paths read the same in-memory cache, so scraping `/metrics` does not trigger additional Zabbix API requests.

> This is an independent community project. It is not affiliated with or endorsed by Zabbix LLC, the Prometheus Authors, or Grafana Labs. All product names and trademarks belong to their respective owners.

## Features

- Zabbix API key or username/password authentication
- Numeric float and numeric unsigned item export
- Filtering by host group, host IP, item-key regular expression, and value type
- `/metrics`, `/internal/metrics`, `/health`, and `/ready` endpoints
- Optional Prometheus Remote Write with bounded queues, paging, spreading, and retries
- Periodic metadata refresh, grouped scheduling, sharded value cache, and bounded expiry
- Kubernetes examples and a Grafana dashboard

## Requirements

- Go 1.25 or newer; use the patched toolchain declared in `go.mod` when possible
- Network access to the Zabbix JSON-RPC API
- A Zabbix API key, or a user allowed to read hosts, host groups, items, and history

## Quick start

```bash
git clone https://github.com/zhaeng/zabbix-exporter.git
cd zabbix-exporter
cp config.example.yaml config.yaml
export ZABBIX_API_KEY='your-zabbix-api-key'
go run ./cmd/server -c config.yaml
```

Check the service:

```bash
curl http://localhost:9110/health
curl http://localhost:9110/ready
curl http://localhost:9110/metrics
```

`config.yaml` is ignored by Git. The configuration file supports `${ENVIRONMENT_VARIABLE}` expansion; supply credentials through environment variables or a secrets manager.

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

- `/metrics`: Zabbix business metrics plus exporter self-metrics
- `/internal/metrics`: exporter, Go runtime, and process metrics only
- `/health`: process liveness
- `/ready`: authentication, metadata, and scheduler readiness

## Remote Write

Push mode is optional. When enabled, exactly one Remote Write endpoint must be configured:

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

API-key authentication takes precedence over username/password authentication. Only use `tls_skip_verify: true` in a controlled environment where installing the internal CA is not possible.

## Build and verify

```bash
make check
./_output/zabbix-exporter --version
```

The CI workflow runs formatting checks, module-file checks, `go vet`, race-enabled tests, builds, and `govulncheck`. CodeQL runs independently.

## Documentation

- [Architecture and data flow](docs/architecture.md)
- [Configuration reference](docs/configuration.md)
- [Self-monitoring metrics](docs/metrics.md)
- [Operations and troubleshooting](docs/operations.md)
- [Kubernetes example](deployments/kubernetes)
- [Grafana dashboard](deployments/zabbix-exporter-grafana-dashboard.json)

## Security

Exported metrics can contain hostnames, IP addresses, and configured labels. HTTP endpoints do not provide authentication. Restrict them with a private network, NetworkPolicy, firewall, or authenticated reverse proxy. Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## Contributing

Issues and pull requests are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) before contributing.

## License

Licensed under the [Apache License 2.0](LICENSE).
