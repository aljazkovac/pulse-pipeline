#!/usr/bin/env python3
"""Pulse Pipeline PR gatekeeper.

Runs the deterministic part of the pre-PR gate and prints a review bundle for
the agent-driven part. Standard library only, so it runs under any agent
runtime or in CI:

    python3 .agents/skills/pr-reviewer/scripts/pr_gatekeeper.py [--base main]

Steps:
  1. Fetch the base branch (best effort) and compute the merge-base.
  2. Collect every change since the merge-base: committed, staged, unstaged,
     and untracked files.
  3. Block on forbidden files (secrets, .env, keys) among those changes.
  4. Run `make test` and `make lint` (plus `make e2e` once web/ exists).
  5. Print the changed files, the diff command, and the review dimensions from
     references/dimensions.md for the reviewer agents.

Exit code 0 means the deterministic checks passed (reviewers still have to
PASS every dimension); 1 means BLOCK; 2 means the script could not run.
"""

from __future__ import annotations

import argparse
import fnmatch
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

SKILL_DIR = Path(__file__).resolve().parent.parent
DIMENSIONS_FILE = SKILL_DIR / "references" / "dimensions.md"

FORBIDDEN_PATTERNS = [
    ".env",
    ".env.*",
    "*.pem",
    "*.key",
    "*credentials*.json",
    "*secret*.json",
]
ALLOWED_PATTERNS = [".env.example"]

OUTPUT_TAIL_LINES = 40


@dataclass
class CheckResult:
    name: str
    passed: bool
    detail: str = ""


def git(repo: Path, *args: str, check: bool = True) -> str:
    proc = subprocess.run(
        ["git", *args], cwd=repo, capture_output=True, text=True, check=False
    )
    if check and proc.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed: {proc.stderr.strip()}")
    return proc.stdout.strip()


def resolve_base(repo: Path, remote: str, base: str, fetch: bool) -> tuple[str, list[str]]:
    """Return the merge-base commit and any warnings about how it was found."""
    warnings: list[str] = []
    if fetch:
        try:
            git(repo, "fetch", "--quiet", remote, base)
        except RuntimeError as err:
            warnings.append(f"could not fetch {remote}/{base}, using local refs: {err}")

    for ref in (f"{remote}/{base}", base):
        if git(repo, "rev-parse", "--verify", "--quiet", ref, check=False):
            return git(repo, "merge-base", ref, "HEAD"), warnings
    raise RuntimeError(f"neither {remote}/{base} nor {base} exists")


def changed_files(repo: Path, merge_base: str) -> list[str]:
    """Files changed since merge_base, including staged, unstaged and untracked."""
    tracked = git(repo, "diff", "--name-only", merge_base).splitlines()
    untracked = git(repo, "ls-files", "--others", "--exclude-standard").splitlines()
    return sorted(set(filter(None, tracked + untracked)))


def check_forbidden_files(files: list[str]) -> CheckResult:
    offending = []
    for path in files:
        name = Path(path).name
        if any(fnmatch.fnmatch(name, p) for p in ALLOWED_PATTERNS):
            continue
        if any(fnmatch.fnmatch(name, p) for p in FORBIDDEN_PATTERNS):
            offending.append(path)
    if offending:
        return CheckResult("forbidden files", False, "\n".join(offending))
    return CheckResult("forbidden files", True)


def run_make(repo: Path, target: str) -> CheckResult:
    name = f"make {target}"
    try:
        proc = subprocess.run(
            ["make", target],
            cwd=repo,
            capture_output=True,
            text=True,
            check=False,
        )
    except FileNotFoundError:
        return CheckResult(name, False, "`make` is not installed")
    if proc.returncode == 0:
        return CheckResult(name, True)
    output = (proc.stdout + proc.stderr).strip().splitlines()
    return CheckResult(name, False, "\n".join(output[-OUTPUT_TAIL_LINES:]))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--base", default="main", help="base branch (default: main)")
    parser.add_argument("--remote", default="origin", help="remote (default: origin)")
    parser.add_argument("--no-fetch", action="store_true", help="do not fetch the base branch")
    parser.add_argument(
        "--skip-make", action="store_true", help="skip make targets (for reviewing docs-only changes)"
    )
    args = parser.parse_args()

    try:
        repo = Path(git(Path.cwd(), "rev-parse", "--show-toplevel"))
        merge_base, warnings = resolve_base(repo, args.remote, args.base, not args.no_fetch)
        files = changed_files(repo, merge_base)
        dimensions = DIMENSIONS_FILE.read_text(encoding="utf-8")
    except (RuntimeError, OSError) as err:
        print(f"gatekeeper error: {err}", file=sys.stderr)
        return 2

    results = [check_forbidden_files(files)]
    if not args.skip_make:
        targets = ["test", "lint"]
        if (repo / "web" / "package.json").exists():
            targets.append("e2e")
        results += [run_make(repo, t) for t in targets]

    print("# PR Gatekeeper Report\n")
    for warning in warnings:
        print(f"WARNING: {warning}")
    print(f"Base: {args.remote}/{args.base} (merge-base {merge_base[:12]})")
    print(f"Review diff: git diff {merge_base}  (plus untracked files below)\n")

    print("## Deterministic checks\n")
    for result in results:
        print(f"- [{'PASS' if result.passed else 'BLOCK'}] {result.name}")
        if result.detail:
            for line in result.detail.splitlines():
                print(f"    {line}")
    if args.skip_make:
        print("- [SKIPPED] make test / make lint (--skip-make)")

    print(f"\n## Changed files ({len(files)})\n")
    for path in files:
        print(f"- {path}")

    print("\n## Review dimensions\n")
    print(dimensions)

    verdict = "PASS" if all(r.passed for r in results) else "BLOCK"
    print(f"Deterministic verdict: {verdict}")
    return 0 if verdict == "PASS" else 1


if __name__ == "__main__":
    sys.exit(main())
