# Pulse Pipeline

**Pulse Pipeline (PP)** is a personal health data platform and cloud-native engineering project. It ingests wearable health telemetry from the **Google Health API** (Fitbit Air) into **PostgreSQL**, exposes analytics via a responsive **Web & Mobile PWA dashboard** and a **Model Context Protocol (MCP)** server, and runs on **Kubernetes** using **ArgoCD**, **Terraform + Terragrunt**, **OpenFeature**, and **Prometheus + Grafana**.

Built for a single user initially, the platform is designed around multi-user isolation and a pluggable `HealthProvider` adapter architecture so it can scale to additional users and wearable providers.

## Tech Stack

- **Backend & Nightly Sync Worker:** Go 1.26+, `pgx`, `sqlc`, `golang-migrate`
- **Database:** PostgreSQL 17
- **Frontend (Desktop + Mobile App):** React, TypeScript, Vite, Tailwind CSS (Progressive Web App)
- **AI / Agentic Integration:** Model Context Protocol (MCP) Go SDK + Custom SDLC Agents
- **Testing:** Go `testing` & Testcontainers, Vitest, Playwright E2E Agents
- **Feature Flags & A/B Testing:** OpenFeature + Flipt
- **Observability:** OpenTelemetry, Prometheus, Grafana
- **Documentation:** Astro Starlight ("Docs as Code")
- **Cloud & GitOps:** Kubernetes (`k3d` locally / DigitalOcean DOKS in cloud), ArgoCD, Terraform + Terragrunt

## Roadmap & Architecture

See [ROADMAP.md](./ROADMAP.md) for the system architecture diagram, technical decision log, and phased implementation checklist.
