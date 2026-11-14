---
name: qa-spec
description: >-
  Acts as the QA & Specification Engineer for Pulse Pipeline. Use this skill when
  writing feature specifications, unit/integration test suites in Go (including
  Testcontainers PostgreSQL tests) and Vitest, or Playwright E2E agent test scenarios
  across desktop and mobile viewports.
---

# Pulse Pipeline — QA & Spec Agent Skill

Use this skill to define executable specifications before or alongside code implementation, and to verify features end-to-end.

## 1. Spec-Driven Testing Pyramid

1. **Unit Tests (Go `testing` & Frontend `Vitest`):**
   - Table-driven tests in Go (`tests := []struct{ name string; ... }`) covering normal paths, boundary timestamps (midnight/DST rollovers, missing wearable intervals), and error paths.
   - Multi-tenant isolation checks: verify User A's queries never return User B's rows.
2. **Integration Tests (`backend/` & `mcp/` with PostgreSQL):**
   - Test real SQL migrations and `sqlc` queries against an isolated PostgreSQL 17 instance (via Testcontainers or local test database).
   - Verify idempotency: running the nightly sync worker twice on the same `MockHealthProvider` dataset must result in identical row counts and values.
3. **End-to-End Tests (`web/` with Playwright Agents):**
   - Every E2E spec in `web/e2e/` must run against **two projects** in `playwright.config.ts`:
     - **Desktop Chrome:** `1280x800` viewport (verifies sidebar navigation and multi-column dashboard grid).
     - **Mobile Pixel 7 / iPhone 14 PWA viewport:** `390x844` viewport (verifies bottom tab navigation, touch targets, and responsive chart rendering).

## 2. Writing a Feature Spec

Before coding a user-facing or ingestion feature, write a concise spec covering:
- **Given / When / Then** acceptance criteria.
- **Edge Cases:** Empty database (new user), partial day data (watch removed for 3 hours), upstream Google Health API `429 Too Many Requests` / `503` retry behavior, expired OAuth token refresh.
