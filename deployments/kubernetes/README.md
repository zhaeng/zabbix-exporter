# Kubernetes Deployment

English | [简体中文](README_zh-CN.md)

These manifests provide secure starting defaults, not a complete production solution. Before deployment, change the image address and Zabbix URL, then adjust resource limits for your environment.

Create the namespace and Secret:

```bash
kubectl apply -f zabbix-namespace.yml
kubectl -n zabbix-exporter create secret generic zabbix-exporter-secrets \
  --from-literal=zabbix-api-key='replace-me'
```

For production, use External Secrets, Sealed Secrets, or your platform's secret manager so credentials do not enter terminal history or YAML files.

Deploy the exporter:

```bash
kubectl apply -f zabbix-configmap.yml
kubectl apply -f zabbix-deployment.yml
kubectl apply -f zabbix-service.yml
kubectl rollout status deployment/zabbix-exporter -n zabbix-exporter
```

Verify it locally:

```bash
kubectl port-forward -n zabbix-exporter service/zabbix-exporter 9110:9110
curl http://127.0.0.1:9110/ready
```

The default Service type is ClusterIP. To expose the exporter outside the cluster, use an Ingress or gateway with access control and TLS.
