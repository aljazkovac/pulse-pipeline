# PR Review Dimensions

Single source of truth for the three gatekeeper review dimensions. `SKILL.md` and
`scripts/pr_gatekeeper.py` both refer to this file; do not copy these checklists elsewhere.

Each dimension is reviewed independently (one reviewer agent per dimension when the
runtime supports sub-agents) and returns `PASS` or `BLOCK` with file/line issues.

## 1. Security & Multi-Tenant Isolation

- Are all SQL queries and API/MCP handlers that touch health data scoped by `user_id`?
  System-context queries must follow the exception in `AGENTS.md` §1.1.
- Are SQL parameters bound (no string-concatenated SQL) and are inputs validated at API/MCP boundaries?
- Are OAuth tokens, secrets, `.env` files, credentials, and personal notes files excluded from the diff and from logs/API responses?

## 2. Architecture, Go & TypeScript Idioms

- Does the diff respect package boundaries (`domain` vs `provider` vs `store` vs `sync`)?
- Are Go errors wrapped with context and is `context.Context` propagated to all I/O?
- Are TypeScript types strict (no `any`) and are UI views responsive on both desktop (`1280x800`) and mobile (`390x844`) viewports?

## 3. Testing & Spec Verification

- Are unit/integration/E2E tests included for new behavior, edge cases, and multi-tenant isolation?
- Are database migrations and sync logic idempotent (safe to re-run for the same date range)?
- Does the change align with `ROADMAP.md` and `AGENTS.md` requirements?
- `make test`, `make lint` (and `make e2e` when `web/` exists) are run by the gatekeeper script; a failure there is a `BLOCK` regardless of reviewer verdicts.
