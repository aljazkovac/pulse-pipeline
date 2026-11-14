# Pulse Pipeline — Infrastructure & GitOps (`infra/`)

Contains:
- `observability/`: Prometheus scrape configurations and Grafana datasource/dashboard provisioning.
- `k8s/`: Kubernetes manifests, Helm charts, and ArgoCD Application definitions (for local `k3d` and DigitalOcean DOKS).
- `terraform/`: Terraform modules and Terragrunt live configurations for DigitalOcean infrastructure.
