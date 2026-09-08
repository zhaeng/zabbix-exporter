# Security Policy

English | [简体中文](SECURITY_zh-CN.md)

## Supported versions

Security fixes are prioritized for the latest release and the `main` branch. Until the project has a stable release, only `main` is maintained.

## Reporting a vulnerability

Use the GitHub repository's **Security → Report a vulnerability** form to report vulnerabilities privately. Do not open a public issue, and do not include production credentials or unnecessary real monitoring data.

A useful report includes:

- the affected version or commit;
- the impact and conditions required for exploitation;
- minimal reproduction steps;
- a suggested fix, if available.

Maintainers will acknowledge the report as soon as practical. Please avoid public disclosure until a fix and release are ready.

## Operational security

- Use a least-privilege Zabbix API key.
- Provide credentials through secrets or environment variables.
- Do not commit `config.yaml`, Kubernetes Secrets, or real metric samples.
- Restrict network access to `/metrics` and health endpoints.
- Do not enable `tls_skip_verify` unless the network is controlled and you understand the risk.
