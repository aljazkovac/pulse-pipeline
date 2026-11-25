# Pulse Pipeline — Web & Mobile PWA (`web/`)

Responsive frontend built with **React**, **TypeScript** (`strict`), **Vite**, and **Tailwind CSS**, to be configured as an installable **Progressive Web App (PWA)** and verified end-to-end with **Playwright** on desktop and mobile viewports. The dashboards follow the [Phase 2 design spec](../docs/design/phase-2-dashboards.md) and the API contract in [ADR-0005](../docs/adr/0005-api-auth-and-read-model.md).

## Local development

Use Node.js 22.22.2 (the CI version), 24.15+ within the 24.x series, or 26+. Later 22.x patches are also supported.

```bash
npm ci
npm run dev        # http://localhost:5173
```

The dev server (and `npm run preview`) proxies `/api` to the Go API, so the browser sees a single origin as in production. The target is `PULSE_API_ORIGIN` if set, otherwise `http://localhost:$API_PORT` (default `8080`) from the repository `.env`. The proxy keeps the browser's `Host` header so session cookies and the API's cross-origin protection see `localhost:5173`.

## Layout

| Path | Contents |
| :--- | :--- |
| `src/` | React app. `index.css` holds the design tokens (`bg-canvas`, `bg-surface`, `text-fg`, `text-fg-muted`, `border-line`, metric accents `text-steps` / `text-heart` / `text-hrv` / `text-sleep`, `text-metric`); dark is the default and light follows `prefers-color-scheme`. |
| `src/api/` | Typed API client (`createApiClient`, `openapi-fetch`) and `schema.d.ts`, generated from [`backend/api/openapi.yaml`](../backend/api/openapi.yaml) — never edit it by hand; run `npm run generate:api` or `make generate`. |
| `src/**/*.test.{ts,tsx}` | Vitest + Testing Library component tests (jsdom). |
| `config/` | Node-side helpers for the Vite config, with their Vitest specs. |
| `e2e/` | Playwright specs (`*.spec.ts`), shared fixtures (`fixtures.ts`, `fixtureUsers.ts`), and the deterministic E2E stack launcher (`stack.ts`); each spec runs in the `desktop` (`1280x800`) and `mobile` (`390x844`, touch) projects with isolated fixture users per project and state. |

TypeScript is split into `tsconfig.app.json` (browser code), `tsconfig.node.json` (configs) and `tsconfig.e2e.json` (Playwright specs), all `strict` with `noUncheckedIndexedAccess` and `exactOptionalPropertyTypes`.

## Make integration

Because `web/package.json` exists, the root `make build`, `make test`, `make lint`, and `make e2e` targets include `web/`, and CI runs them. The package defines these scripts:

| Script | Purpose |
| :--- | :--- |
| `build` | Type-check, then the production Vite bundle. |
| `typecheck` | `tsc -b` across the three projects (no emit, `"strict": true`). |
| `lint` | ESLint (`typescript-eslint` strict type-checked, React Hooks), `@typescript-eslint/no-explicit-any` as an error, zero warnings. |
| `test` | Vitest unit/component tests. |
| `test:e2e` | Playwright specs across the desktop (`1280x800`) and mobile (`390x844`) projects against the full deterministic stack (`e2e:stack`). Install the browser once with `npx playwright install chromium`. |
| `e2e:stack` | Starts compose PostgreSQL, compiles `pulse-worker` and `pulse-api`, seeds per-project fixture users at `E2E_DEV_NOW`, and runs polling `pulse-worker` (`1s`) + `pulse-api` (`ENV=development`, `PULSE_ENABLE_DEV_LOGIN=true`) + Vite (`:5173`). |

`generate:api` regenerates `src/api/schema.d.ts` from the OpenAPI contract; `make generate-check` fails CI when it is stale. `openapi-typescript` declares a TypeScript 5 peer; `package.json` `overrides` points it at the project's TypeScript 6, which it only uses to print declarations.

The package manager defaults to `npm`; override with `make test WEB_PM=pnpm`.
