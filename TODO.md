# Pulse Pipeline

## Requirements

Pulse Pipeline (PP) is a personal health data platform, initially built only for me, but must be designed so that it could be scaled to other users and other data providers.
PP is both a personal software product (that could potentially be open-sourced or monetized later), and also a learning project.

### Building blocks

#### Architecture

- [ ] Create a service that pulls health data from the Google Health API on a nightly basis and displays it in dashboards (backend, frontend, database)
- [ ] Must be accessible both on a desktop web browser and as a phone app
- [ ] Write tests for the backend, frontend and the MCP: unit, integration, E2E -> use specs and Playwright agents
- [ ] Implement feature flags and A/B testing
- [ ] Add observability via Prometheus and Grafana
- [ ] Create documentation (both for the developers and the users) and use a documentation tool to display it somewhere
- [ ] Host everything in the cloud, using Kubernetes and ArgoCD and Terraform with Terragrunt
- [ ] Set up observability using Prometheus and Grafana

#### Agentic

- [ ] Create an MCP wrapper to query and display the data
- [ ] Create various agents and experiment with quality, production process, and gatekeeping of the code: designer, PR review, architect, coder, etc.
