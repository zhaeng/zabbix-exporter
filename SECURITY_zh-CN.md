# 安全策略

[English](SECURITY.md) | 简体中文

## 支持范围

安全修复优先应用于最新发布版本和 `main` 分支。项目尚未发布稳定版本时，仅维护 `main`。

## 报告漏洞

请使用 GitHub 仓库的 **Security → Report a vulnerability** 私下报告。不要创建公开 Issue，也不要在报告中包含生产凭据或不必要的真实监控数据。

报告建议包含：

- 受影响版本或提交
- 影响和可利用条件
- 最小复现步骤
- 建议修复方式（如有）

维护者会尽快确认收到报告，在完成修复和发布前请避免公开披露。

## 运维安全

- 使用最小权限的 Zabbix API key。
- 通过 Secret 或环境变量提供凭据。
- 不要将 `config.yaml`、Kubernetes Secret 或真实指标样本提交到 Git。
- 限制 `/metrics` 和健康端点的网络访问。
- 除非处于受控网络且理解风险，否则不要启用 `tls_skip_verify`。
