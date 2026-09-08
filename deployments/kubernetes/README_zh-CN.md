# Kubernetes 部署

[English](README.md) | 简体中文

这些清单是安全默认值的起点，不是完整生产方案。部署前请修改镜像地址和 Zabbix URL，并根据环境调整资源限制。

创建命名空间和 Secret：

```bash
kubectl apply -f zabbix-namespace.yml
kubectl -n zabbix-exporter create secret generic zabbix-exporter-secrets \
  --from-literal=zabbix-api-key='replace-me'
```

建议在生产环境使用 External Secrets、Sealed Secrets 或平台密钥管理服务，避免凭据进入终端历史和 YAML 文件。

部署：

```bash
kubectl apply -f zabbix-configmap.yml
kubectl apply -f zabbix-deployment.yml
kubectl apply -f zabbix-service.yml
kubectl rollout status deployment/zabbix-exporter -n zabbix-exporter
```

本地检查：

```bash
kubectl port-forward -n zabbix-exporter service/zabbix-exporter 9110:9110
curl http://127.0.0.1:9110/ready
```

默认 Service 是 ClusterIP。需要从集群外暴露时，请使用带访问控制和 TLS 的 Ingress 或网关。
