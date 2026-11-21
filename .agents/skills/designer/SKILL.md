---
name: designer
description: >-
  Acts as the Product & UI/UX Designer for Pulse Pipeline. Use this skill when
  designing or reviewing web and mobile PWA health dashboards, chart visualizations,
  responsive navigation shells, Tailwind CSS theme tokens, or accessibility (a11y)
  in the web/ frontend.
---

# Pulse Pipeline — Designer Agent Skill

Use this skill when planning or building frontend views, components, and charts in `web/`.

## Design System & UX Guidelines

1. **Dual-Viewport Responsive Shell (Desktop Web + Mobile PWA):**
   - **Desktop (`>= 768px`):** Persistent left sidebar navigation (`240px` width) with multi-column Bento-grid dashboard cards (`max-w-7xl`).
   - **Mobile PWA (`< 768px`):** Fixed bottom tab navigation bar with iOS/Android safe-area inset padding (`pb-[env(safe-area-inset-bottom)]`), touch targets at least `44x44px`, and single-column scrollable metric cards.
2. **Visual Hierarchy for Health Telemetry:**
   - **At-a-Glance Summary First:** Every dashboard view starts with high-signal KPI cards (current value, unit, delta vs. 7-day baseline, and semantic status badge).
   - **Semantic Metric Palette:**
     - **Heart Rate / Cardio:** Rose/Coral (`rose-500`)
     - **Sleep / Recovery:** Indigo/Violet (`indigo-500`, with distinct shades for Deep, REM, Light, Awake stages)
     - **Activity / Steps:** Emerald/Teal (`emerald-500`)
     - **HRV / Readiness:** Sky/Cyan (`sky-500`)
   - **Dark Mode Default:** Health dashboards should look crisp and restful in dark mode (`slate-950` background, `slate-900` elevated cards, subtle `slate-800` borders) while supporting light mode cleanly.
3. **Resilient UI States:**
   - Every data-driven component must explicitly define 4 states:
     1. **Loading:** Skeleton shimmer matching the card's exact dimensions (zero layout shift / CLS).
     2. **Empty (No Data / Not Synced Yet):** Clear call-to-action to connect Google Health or trigger a sync.
     3. **Populated:** Crisp typography (`tabular-nums` on all numeric metrics so digits align) and interactive chart tooltips.
     4. **Error:** Non-intrusive inline error banner with a "Retry" button.
4. **Accessibility & Playwright Testability:**
   - Use semantic HTML landmarks (`<nav>`, `<main>`, `<section aria-labelledby="...">`).
   - Provide accessible `aria-label` or `data-testid` attributes on interactive controls and chart containers so Playwright agents can inspect them deterministically.
