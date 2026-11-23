/**
 * Placeholder app root. The responsive layout shell (sidebar / bottom tab bar,
 * signed-in and signed-out states) replaces this in the next Phase 2a step; it
 * already carries the landmarks every view keeps: skip link, one <main>, one <h1>.
 */
export function App() {
  return (
    <>
      <a
        href="#main"
        className="sr-only rounded-md bg-surface px-4 py-3 text-fg focus:not-sr-only focus:fixed focus:top-4 focus:left-4 focus:z-50"
      >
        Skip to main content
      </a>
      <main id="main" tabIndex={-1} className="mx-auto max-w-7xl px-4 py-8 focus:outline-none">
        <h1 className="text-2xl font-semibold">Pulse Pipeline</h1>
        <p className="mt-2 text-fg-muted">
          Your wearable health data, on desktop and phone. Dashboards arrive with the Phase 2
          slices.
        </p>
      </main>
    </>
  );
}
