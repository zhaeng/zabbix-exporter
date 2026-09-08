# C01 scheduled 配置迁移与版本回滚

## 适用范围

C01 删除后，服务只有 `metadata → scheduler → expiry → streaming publisher` 一条运行路径。`prometheus.push.enabled: false` 会关闭 Remote Write publisher，但 metadata、scheduler、expiry、ValueCache 和 `/metrics` 仍正常运行。

本次删除由用户显式豁免 T09、T10 和生产稳定观察门禁后执行，只授权本地代码、测试和文档变更，不代表生产切换、部署或发布授权。删除前版本回滚点是 master `1b44ae7`，其中代码基线为 `5bc53a4`。

## 配置迁移

迁移前备份实际配置；不要把含凭据的 `config.yaml` 提交到仓库。

从 YAML 中删除以下旧字段：

- `collector.pipeline`
- `prometheus.push.streaming_enabled`
- `collector.max_concurrent_hosts`
- `cache.refresh_interval`
- Remote Write endpoint 下的 `max_shards`

当前使用 `yaml.v3` 的宽松解析，以上残留字段会被忽略，因此可以先升级程序、再清理配置。它们不再选择采集或推送路径。

Push 关闭时可不配置 endpoint：

```yaml
prometheus:
  pull:
    enabled: true
    port: 9110
    path: /metrics
  push:
    enabled: false
```

Push 开启时必须且只能配置一个 endpoint：

```yaml
prometheus:
  push:
    enabled: true
    interval: 60
    remote_write:
      endpoints:
        - url: http://remote-write.example/api/v1/write
          timeout: 30s
          max_samples_per_send: 2000
```

迁移后先用隔离的 mock Zabbix 和本机 Remote Write 接收器验证。不得把该验证命令指向真实 Remote Write。

## 本地版本回滚步骤

以下步骤在新的临时 worktree 中执行，不修改当前工作树：

```bash
git worktree add /tmp/zabbix-exporter-pre-c01 1b44ae7
cd /tmp/zabbix-exporter-pre-c01
go test ./...
go build ./cmd/server
```

使用迁移前备份的配置，或恢复旧版本识别的字段。旧版本未显式配置 `collector.pipeline` 时默认走 legacy；若要恢复删除前 scheduled 路径，应恢复 `collector.pipeline: scheduled`，且启用 Push 时恢复 `prometheus.push.streaming_enabled: true`。确认本地验证完成后可删除临时 worktree：

```bash
git worktree remove /tmp/zabbix-exporter-pre-c01
```

生产回滚还必须使用部署前实际保留并验证过的镜像、配置备份和平台发布流程。本次 C01 没有构建、发布或验证生产镜像，也没有演练生产回滚，不能把上述本地演练视为生产回滚证明。
