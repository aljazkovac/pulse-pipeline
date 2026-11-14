# Dynamic workflow script for Antigravity `run_workflow` tool.
# Fans out 3 specialized gatekeeper agents in parallel to review the current branch diff against main.

phase("Parallel PR Gatekeeper Audit")

review_angles = [
    {
        "role": "Security & Tenancy Gatekeeper",
        "focus": (
            "Audit git diff origin/main...HEAD for: "
            "(1) strict user_id multi-tenant scoping in all queries/handlers, "
            "(2) no leaked secrets, .env files, OAuth tokens, or unignored personal files, "
            "(3) input validation and safe credential handling."
        ),
    },
    {
        "role": "Architecture & Idioms Gatekeeper",
        "focus": (
            "Audit git diff origin/main...HEAD for: "
            "(1) clean separation between domain, HealthProvider adapters, and store packages, "
            "(2) idiomatic Go error wrapping and context propagation, "
            "(3) strict TypeScript typing and responsive Desktop + Mobile PWA design."
        ),
    },
    {
        "role": "QA & Spec Coverage Gatekeeper",
        "focus": (
            "Audit git diff origin/main...HEAD for: "
            "(1) unit/integration/E2E test coverage matching new code paths, "
            "(2) idempotency of database migrations and sync logic, "
            "(3) alignment with ROADMAP.md and AGENTS.md requirements."
        ),
    },
]

finding_schema = {
    "type": "object",
    "properties": {
        "verdict": {"type": "string", "enum": ["PASS", "BLOCK"]},
        "summary": {"type": "string"},
        "issues": {
            "type": "array",
            "items": {"type": "string"},
        },
    },
    "required": ["verdict", "summary", "issues"],
}

results = await parallel(
    review_angles,
    lambda angle: agent(
        prompt=(
            f"You are the {angle['role']} for Pulse Pipeline.\n"
            f"Inspect the repository changes on the current branch compared to origin/main.\n"
            f"Focus specifically on: {angle['focus']}\n"
            f"Return PASS if no blocking issues exist, or BLOCK with specific file/line issues."
        ),
        schema=finding_schema,
        role=angle["role"],
        workspace="inherit",
    ),
)

phase("Verdict Aggregation")

overall_pass = all(r["verdict"] == "PASS" for r in results)
for angle, res in zip(review_angles, results):
    log(f"[{res['verdict']}] {angle['role']}: {res['summary']}")
    for issue in res.get("issues", []):
        log(f"  - {issue}")

print({
    "overall_verdict": "PASS" if overall_pass else "BLOCK",
    "reviews": results,
})
