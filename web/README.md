# Pulse Pipeline — Web & Mobile PWA (`web/`)

Responsive frontend built with **React**, **TypeScript**, **Vite**, and **Tailwind CSS**, configured as an installable **Progressive Web App (PWA)** for mobile devices and verified end-to-end with **Playwright agents**.

## Make Integration

Once `web/package.json` exists, the root `make build`, `make test`, `make lint`, and `make e2e` targets include `web/`. The package must define these scripts:

| Script | Purpose |
| :--- | :--- |
| `build` | Production Vite/PWA bundle. |
| `typecheck` | `tsc --noEmit` with `"strict": true`. |
| `lint` | ESLint, including `@typescript-eslint/no-explicit-any` as an error. |
| `test` | Vitest unit/component tests. |
| `test:e2e` | Playwright specs across the desktop (`1280x800`) and mobile (`390x844`) projects. |

The package manager defaults to `npm`; override with `make test WEB_PM=pnpm`.
