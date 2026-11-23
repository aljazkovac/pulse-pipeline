# Phase 2 dashboard design

The first release lets a signed-in user answer two questions: “What did my wearable record for this day?” and “When did this provider last sync?” All dates and times use the profile timezone. Show the selected date, provider and timezone beside the data. The [API decision](../adr/0005-api-auth-and-read-model.md) defines the read model and wire formats.

## Shell and tokens

At 768 px and wider, use a 240 px sidebar and a two-column card grid within a max-w-7xl main region. Below that breakpoint use one column and a fixed bottom navigation with safe-area padding. Reserve its height in the scroll area so the final action remains reachable. Ship Overview and Connections in 2a. Add Heart and Sleep as their slices ship. Settings opens from Overview. On sign-out or session expiry, clear visible health data and show the Google sign-in action. Signing in and linking a wearable are separate actions.

| Token | Use |
| --- | --- |
| Canvas / surface / text | Dark default: slate-950 / slate-900 / slate-100. Light: slate-50 / white / slate-900. |
| Metric accents | Emerald for steps, rose for heart rate, sky for HRV, indigo for sleep. Pair each with a label and unit. |
| Type and spacing | 16 px body, 24 px headings, 32 px tabular metric values. Use 8 px spacing increments and 16 px card padding. |
| Interaction | Minimum 44 × 44 px target, visible contrasting focus ring, underlined text links. |

Use one main heading, labelled sections, a skip link and a navigation landmark with the current page marked. On navigation move focus to the new heading. Give chart series a text summary and a keyboard-accessible value table. Respect reduced motion.

## Views and data

Paths below are relative to the session-scoped API v1. Use the server's derived values. Keep missing intervals visible as gaps.

| View | Read and interaction |
| --- | --- |
| Overview | Open on today in the profile timezone. A labelled date picker reloads GET /overview for that day. Show steps and goal as numbers beside the ring (cap the arc at 100% while retaining the actual count), sleep duration in hours and minutes, resting HR in bpm, HRV in ms and last sync. Link sync status to Connections. |
| Settings | Populate from GET /me. Save timezone, primary provider and integer step goal (1–100000) with PATCH /me. Keep invalid inputs editable with inline errors. After success refresh the profile and affected day in the saved timezone. |
| Connections | GET /connections supplies provider status and last/next sync. Show each provider's recent sync-runs in descending time order. Offer Link Google Health for a new account and Re-link when required. After returning from consent refresh the list and show the outcome. |
| Sync now | POST to the selected connection's sync endpoint. Render the returned queued or running state immediately. Refresh connections and runs while pending, distinguish a pending request from an unrelated running backfill, and announce transitions once. Show the terminal result and time. On relink-required offer Re-link. |
| Heart & HRV | Use /metrics/{metric}/series for the selected day's heart rate timeline, and /daily for 7/30/90-day resting HR and HRV trends. Label units and distinguish the trailing seven-day mean with a labelled dashed line. Show bucket times in the profile timezone. |
| Sleep | Use /sleep for sessions by local wake date. Show time asleep, time in bed, and labelled Deep, REM, Light and Awake durations. For unstaged sleep show reported time asleep when available. Show bedtime and wake consistency separately in minutes when calculable. |

## States and acceptance

While loading, reserve the populated card dimensions, mark the section busy and hide decorative skeletons from screen readers. For a failed read show an inline Retry for that section and retain its selected date. Offline, show a connection message and reload on reconnect. Only the app shell may be cached. A missing metric reads “No data”; a reported zero displays its value and unit.

Run each scenario at 1280 × 800 and 390 × 844 with a fixed seed user and clock. Use accessible roles and names for assertions.

| Given | When | Expected |
| --- | --- | --- |
| Signed out | Open Overview | Google sign-in is reachable by keyboard and no metric is visible. |
| Signed in without connections | Open Overview | Show Link Google Health. With an active connection but no records, offer Sync now through Connections. |
| A seeded day and a partially recorded day | Change dates | Values update for the selected date. Missing HRV leaves the other cards usable. An unstaged session with unknown duration shows No data; reported zero shows 0 min. |
| An active connection | Submit Sync now twice before the worker claims it | One pending request is shown. After claim show Running, then the run outcome. A failed run remains visible in history. |
| A revoked connection | Open Connections | Re-link is reachable without opening developer tools. Cancelling consent leaves the existing status visible. |
| A failed request | Retry after connectivity returns | The selected date and provider persist and the affected section recovers. |
| Midnight in a different browser timezone | Open a day with overnight sleep | Overview and Sleep assign the session to the same profile-local wake date. |
| Keyboard or touch navigation | Open each shipped view and reach its final control | Current page, focus and chart values are discernible. No horizontal page scroll or obscured control at either viewport. |
