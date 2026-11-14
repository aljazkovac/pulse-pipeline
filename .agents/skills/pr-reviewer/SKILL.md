---
name: pr-reviewer
description: >-
  Acts as the PR Reviewer & Quality Gatekeeper for Pulse Pipeline. Use this skill
  before opening or merging a Pull Request to audit code diffs for security,
  multi-tenant user_id isolation, Go/TypeScript best practices, and test coverage,
  including running the gatekeeper script and the per-dimension reviews.
---

# Pulse Pipeline — PR Reviewer & Gatekeeper Skill

Use this skill to perform a rigorous pre-PR or pre-merge audit of all changes on the
current branch — committed, staged, unstaged, and untracked — against `main`.

## 1. Run the Gatekeeper Script

From the repository root:

```bash
python3 .agents/skills/pr-reviewer/scripts/pr_gatekeeper.py
```

The script ([pr_gatekeeper.py](./scripts/pr_gatekeeper.py)) uses only the Python standard
library and works in any agent runtime or CI. It:

- fetches `origin/main` and reviews everything since the merge-base, including uncommitted work;
- blocks on forbidden files (`.env`, keys, credential JSON) among the changes;
- runs `make test` and `make lint` (and `make e2e` once `web/` exists);
- prints the changed files, the exact `git diff` command to review, and the review dimensions.

Exit code `1` (BLOCK) or `2` (could not run) stops the review: fix the cause and re-run.

## 2. Review Each Dimension

The dimensions are defined only in [references/dimensions.md](./references/dimensions.md).
Review the diff printed by the script against each dimension separately. If your runtime
supports sub-agents, run one reviewer per dimension in parallel; otherwise review them in sequence.

Each dimension returns `PASS` or `BLOCK` with specific file/line issues.

## 3. Verdict

The overall verdict is `PASS` only if the script exits `0` **and** every dimension returns
`PASS`. A reviewer that fails, times out, or returns no verdict counts as `BLOCK` — re-run
that dimension rather than skipping it. Report all dimensions' findings, not only the first `BLOCK`.
