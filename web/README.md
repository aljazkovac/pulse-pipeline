# Pulse Pipeline — Web & Mobile PWA (`web/`)

Responsive frontend built with **React**, **TypeScript** (`strict`), **Vite**, and **Tailwind CSS**, to be configured as an installable **Progressive Web App (PWA)** and verified end-to-end with **Playwright** on desktop and mobile viewports. The dashboards follow the [Phase 2 design spec](../docs/design/phase-2-dashboards.md) and the API contract in [ADR-0005](../docs/adr/0005-api-auth-and-read-model.md).

## Local development

Requires Node.js 22.12+.

```bash
npm ci
npm run dev        # http://localhost:5173
```

The dev server (and `npm run preview`) proxies `/api` to the Go API, so the browser sees a single origin as in production. The target is `PULSE_API_ORIGIN` if set, otherwise `http://localhost:$API_PORT` (default `8080`) from the repository `.env`. The proxy keeps the browser's `Host` header so session cookies and the API's cross-origin protection see `localhost:5173`.

## Layout

| Path | Contents |
| :--- | :--- |
| `src/` | React app. `index.css` holds the design tokens (`bg-canvas`, `bg-surface`, `text-fg`, `text-fg-muted`, `border-line`, metric accents `text-steps` / `text-heart` / `text-hrv` / `text-sleep`, `text-metric`); dark is the default and light follows `prefers-color-scheme`. |
| `src/**/*.test.tsx` | Vitest + Testing Library component tests (jsdom). |
| `config/` | Node-side helpers for the Vite config, with their Vitest specs. |
| `e2e/` | Playwright specs; each runs in the `desktop` (`1280x800`) and `mobile` (`390x844`, touch) projects. |

TypeScript is split into `tsconfig.app.json` (browser code), `tsconfig.node.json` (configs) and `tsconfig.e2e.json` (Playwright specs), all `strict` with `noUncheckedIndexedAccess` and `exactOptionalPropertyTypes`.

## Make integration

Because `web/package.json` exists, the root `make build`, `make test`, `make lint`, and `make e2e` targets include `web/`, and CI runs them. The package defines these scripts:

| Script | Purpose |
| :--- | :--- |
| `build` | Type-check, then the production Vite bundle. |
| `typecheck` | `tsc -b` across the three projects (no emit, `"strict": true`). |
| `lint` | ESLint (`typescript-eslint` strict type-checked, React Hooks), `@typescript-eslint/no-explicit-any` as an error, zero warnings. |
| `test` | Vitest unit/component tests. |
| `test:e2e` | Playwright specs across the desktop (`1280x800`) and mobile (`390x844`) projects; starts the Vite dev server itself. Install the browser once with `npx playwright install chromium`. |

The package manager defaults to `npm`; override with `make test WEB_PM=pnpm`.
