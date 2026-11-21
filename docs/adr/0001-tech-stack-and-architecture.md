# ADR 0001: Core Technology Stack & Multi-Tenant Provider Architecture

- **Status:** Accepted
- **Date:** 2026-11-14

## Context

Pulse Pipeline (PP) is a personal health telemetry platform built initially to ingest wearable data for a single user (Fitbit Air via the **Google Health API**), display analytics on both desktop web browsers and mobile devices, expose health data to AI assistants via **Model Context Protocol (MCP)**, and run on cloud-native infrastructure (**Kubernetes**, **ArgoCD**, **Terraform + Terragrunt**, **OpenTelemetry**, **Prometheus + Grafana**, **OpenFeature**).

Although initially single-user, the system must be designed so that it can scale to multiple users and additional wearable data providers without architectural rewrites. It also serves as a deliberate engineering learning project outside our daily .NET stack.

## Decision

1. **Backend & MCP Language — Go (1.23+):**
   - We choose **Go** for the backend API (`backend/cmd/api`), nightly ingestion worker (`backend/cmd/worker`), and MCP server (`mcp/cmd/mcp`).
   - Go provides compile-time static type safety, native REST and gRPC support for the modernized Google Health API, official MCP SDK support (`modelcontextprotocol/go-sdk`), tiny container images, and native alignment with the Kubernetes/Prometheus ecosystem.

2. **Database — Standard PostgreSQL 17 (`pgx` + `sqlc` + `golang-migrate`):**
   - We choose standard **PostgreSQL 17** over specialized time-series forks or NoSQL stores.
   - With composite unique keys such as `(user_id, provider, metric_type, recorded_at)` that serve both dashboard reads and idempotent `ON CONFLICT DO UPDATE` upserts, standard PostgreSQL easily handles millions of intraday rows while remaining 100% portable across local Docker and managed cloud databases (DigitalOcean Managed PostgreSQL).

3. **Desktop + Phone App Strategy — Responsive React/TypeScript Web App + PWA:**
   - We build a single responsive web application (`web/`) using **React**, **TypeScript**, **Vite**, and **Tailwind CSS**, configured with a **Progressive Web App (PWA)** manifest and service worker.
   - This provides a desktop browser dashboard and an installable full-screen mobile home-screen app without Xcode/Android Studio build friction, while keeping 100% of the UI in standard HTML DOM so **Playwright E2E agents** can verify both desktop and mobile viewports natively.
   - If native App Store / Play Store packaging is ever needed later, the exact same web build can be wrapped in **Capacitor**.

4. **Multi-User & Multi-Provider Domain Model:**
   - Every health data table and query is scoped by `user_id` from day one.
   - External wearable APIs are decoupled behind a Go `HealthProvider` interface, starting with `MockHealthProvider` (for deterministic tests) and `GoogleHealthProvider` (Google OAuth 2.0 + Google Health API).

5. **Cloud & GitOps Target — DigitalOcean (DOKS) + GCP (OAuth Only):**
   - We use a free-tier **Google Cloud Project** strictly for Google Health API & OAuth 2.0 credentials, and target **DigitalOcean Kubernetes (DOKS)** (plus local `k3d`) via **Terraform + Terragrunt** and **ArgoCD** for predictable personal cloud costs.

## Consequences

- **Positive:** Compile-time type safety across SQL (`sqlc`), Go (`backend/` & `mcp/`), and TypeScript (`web/`); zero duplicate UI codebases between desktop and phone; full compatibility with Playwright E2E agents; low memory footprint on Kubernetes.
- **Trade-offs:** Standard PostgreSQL requires explicit SQL `date_trunc` / bucketing queries rather than TimescaleDB-specific helper functions, which is an acceptable trade-off for cloud portability.
