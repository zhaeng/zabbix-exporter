# Contributing Guide

English | [简体中文](CONTRIBUTING_zh-CN.md)

Thank you for contributing to Zabbix Exporter.

## Before you begin

1. Search existing issues to avoid duplicate work.
2. Open an issue before implementing a large feature, protocol change, or backward-incompatible change.
3. Do not report security vulnerabilities in a public issue; follow [SECURITY.md](SECURITY.md).

## Local development

```bash
git clone https://github.com/zhaeng/zabbix-exporter.git
cd zabbix-exporter
go mod download
make check
```

Tests must not depend on a production Zabbix instance, real credentials, or a public Remote Write service. Test new behavior with mock HTTP servers, fixtures, or a fake clock.

## Pull request requirements

- Keep each pull request focused on one problem and describe the motivation, behavior change, and verification performed.
- Include tests for new features and bug fixes.
- Update the README, example configuration, and CHANGELOG when configuration or user-visible behavior changes.
- Never commit passwords, tokens, API keys, internal addresses, or real monitoring data.
- Ensure `make check` passes.
- Contributions are provided under this repository's Apache-2.0 license.

Maintainers may ask that an oversized pull request be split, or request compatibility and performance evidence before merging.
