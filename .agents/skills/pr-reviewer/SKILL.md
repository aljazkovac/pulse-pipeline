---
name: pr-reviewer
description: >-
  Acts as the PR Reviewer & Quality Gatekeeper for Pulse Pipeline. Use this skill
  before opening or merging a Pull Request to audit code diffs for security,
  multi-tenant user_id isolation, Go/TypeScript best practices, and test coverage,
  including running the parallel multi-agent gatekeeper workflow.
---

# Pulse Pipeline — PR Reviewer & Gatekeeper Skill

Use this skill to perform a rigorous pre-PR or pre-merge audit of staged/branch changes.

## 1. Gatekeeping Dimensions

Every PR is evaluated across three non-negotiable dimensions:

1. **Security & Multi-Tenant Isolation (`Security Reviewer`):**
   - Are all SQL queries and API/MCP handlers strictly scoped by `user_id`?
   - Are OAuth tokens, secrets, `.env` files, or credentials excluded from the diff and logs?
   - Are inputs validated and SQL parameters strictly bound?
2. **Architecture, Go & TypeScript Idioms (`Code Quality Reviewer`):**
   - Does the diff respect package boundaries (`domain` vs `provider` vs `store`)?
   - Are Go errors wrapped with context and `context.Context` propagated properly?
   - Are TypeScript types strict (`no-explicit-any`) and UI components responsive for both desktop and mobile PWA viewports?
3. **Testing & Spec Verification (`QA Gatekeeper`):**
   - Are unit/integration/E2E tests included for new behavior and edge cases?
   - Do `make test` and `make lint` pass cleanly?

## 2. Running the Parallel Multi-Agent Gatekeeper Workflow

For non-trivial PRs, run the automated gatekeeper workflow script using the `run_workflow` tool:

- **Script Path:** [pr_gatekeeper.py](./scripts/pr_gatekeeper.py)
- **How to invoke:**
  Call `run_workflow` with `ScriptPath` set to the absolute path of `.agents/skills/pr-reviewer/scripts/pr_gatekeeper.py` and `Description="Run parallel PR Gatekeeper review"`.
- **What it does:**
  Spawns three specialized review agents in parallel (`Security & Multi-Tenancy`, `Architecture & Code Quality`, `QA & Test Coverage`) to inspect the current branch diff against `main`, and aggregates their findings into a `PASS` or `BLOCK` verdict.
