# 贡献指南

感谢你参与 Zabbix Exporter。

## 开始之前

1. 搜索现有 Issue，避免重复工作。
2. 对较大的功能、协议变化或不兼容修改，先创建 Issue 讨论方案。
3. 安全漏洞不要提交公开 Issue，请遵循 [SECURITY.md](SECURITY.md)。

## 本地开发

```bash
git clone https://github.com/zhaeng/zabbix-exporter.git
cd zabbix-exporter
go mod download
make check
```

测试不得依赖生产 Zabbix、真实凭据或公网 Remote Write 服务。新增行为应使用 mock HTTP server、fixture 或 fake clock 验证。

## Pull Request 要求

- 每个 PR 聚焦一个问题，说明动机、行为变化和验证方式。
- 新功能和缺陷修复应包含测试。
- 配置或用户可见行为变化应同步更新 README、示例配置和 CHANGELOG。
- 不得提交密码、token、API key、内部地址或真实监控数据。
- 提交应能通过 `make check`。
- 贡献内容按照本仓库的 Apache-2.0 许可证提供。

维护者可能要求拆分过大的 PR，或在合并前补充兼容性和性能证据。
