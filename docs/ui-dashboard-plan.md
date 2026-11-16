# SOAR web console in one binary: design and plan

Status: **locked** (2026-11-16). Agreed in a grilling session that went through every branch of the design.
Branch: `ui-dashboard` (off `develop`). Nothing is committed until the owner asks.

## Goal

Ship a polished React console for SOAR (spec section 12) inside the `soar` binary, so one build produces one
artifact that serves both the API and the UI. Today the UI is a separate nginx container (`soar-web`) and the
pages are functional but plain. Triage is also limited: alerts cannot be acknowledged, closed or assigned.

## Current state

* `web/` holds a working React 18 + Vite 5 + Tailwind 3 UI (about 6.6k lines): all 10 pages of spec 12, OIDC
  (oidc-client-ts, PKCE) and API-key sign-in, an openapi-fetch client generated from `api/openapi.yaml`, SSE
  live updates (`/stream?access_token=`), and DSL <-> React Flow canvas sync with tests.
* nginx (`deploy/web/`, `deploy/docker/web.Dockerfile`) serves it on `127.0.0.1:3000`, proxies `/api`, and
  writes `/config.json` from env.
* `soar` is one Go binary with roles (`api`, `ingest`, `processor`, ...), each run as its own process.
* `GET /dashboard` returns 24 h totals only (no time series, no top-N).
* Alerts have `status` and `verdict` fields but no update endpoint; the only alert action is
  `POST /alerts/{id}/case`. Case `assignee` is free text. OIDC users are not principals (no JIT provisioning),
  and analysts lack `principals:read`.
* Alert dedup groups by fingerprint in fixed windows (default 600 s, `processor.AlertID`).
* Retention is 7 days for all data (spec 5.2).

## Locked decisions

### Delivery

1. **Rework `web/` in place, no rewrite.** Keep `lib/` (api, auth, stream, dsl) and the canvas logic; rebuild
   the visual layer and each page's layout. The working logic and its tests are worth more than a clean slate.
2. **Embed the UI in the `soar` binary, served by the `api` role.** `go:embed` of the Vite build. `make build`
   runs the UI build, then `go build` into `bin/soar`. `web/dist` stays out of git. A plain `go build` /
   `go test ./...` without Node embeds a small "UI not built, run `make build`" page, so Go CI never needs
   Node. The `soar` Dockerfile gains a Node build stage.
3. **Retire nginx.** Delete `deploy/web/`, `deploy/docker/web.Dockerfile`, the `image-web` make target and the
   `web` compose service. `make web-dev` (Vite with hot reload, proxying `/api`) stays for development.
4. **No `soar all` role.** One process for every role is backend work outside this scope; the UI ships with
   `soar api` next to the other roles, as today. Possible follow-up.

### Serving (Go)

5. `soar api` serves the SPA at `/` and the API at `/api/v1` on the same port (:8080). Same origin: no CORS,
   no proxy.
6. `SOAR_UI_ENABLED` (default true) turns the UI off.
7. `/config.json` is built at request time from env: `SOAR_UI_OIDC_AUTHORITY` (default `SOAR_OIDC_ISSUER`),
   `SOAR_UI_OIDC_CLIENT_ID` (default `soar-web`). One binary works in every environment.
8. Any non-`/api` path with no matching file returns `index.html` (client-side routes). Hashed `/assets/*`:
   `Cache-Control: public, max-age=31536000, immutable`. `index.html` and `config.json`: `no-cache`.
9. Security headers replace nginx's: CSP with `default-src 'self'`, `script-src 'self'`, workers from `'self'`
   and `blob:` (Monaco), `connect-src 'self'` plus the OIDC authority's origin, `frame-ancestors 'none'`;
   also `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`. No external
   CDN: fonts and Monaco are bundled.
10. The Keycloak realm (`deploy/keycloak/realm.json`) redirect URIs and web origins move from `:3000` to
    `:8080`.
11. **Remote access:** `SOAR_API_PUBLISH` (default `127.0.0.1`) in compose, the same pattern as
    `SOAR_INGEST_PUBLISH`. For the lab: set it to the LAN address and sign in with an API key over plain HTTP.
    OIDC (PKCE) needs a secure context, so Keycloak login works only on `localhost` (SSH tunnel:
    `ssh -L 8080:127.0.0.1:8080 -L 8180:127.0.0.1:8180`) or HTTPS. HTTPS on the API (`SOAR_TLS_CERT_FILE`)
    comes later, alongside HTTPS ingest. This matches the earlier lab-first choice for Wazuh.
12. **Auth model unchanged:** bearer token (OIDC or API key) in sessionStorage; SSE takes `?access_token=`.
    Cookie sessions would be safer against XSS but are a real backend auth change; possible security step later.

### Backend additions (contract-first in `api/openapi.yaml`, with Go tests)

Additive and read-only, with one agreed exception for alert triage (13) and two behavior changes (16).

13. **`PATCH /alerts/{id}`** (`alerts:write`): `status` in `new | acknowledged | in_progress | closed`,
    `verdict` in `true_positive | false_positive | benign | undetermined`, `assignee` (new string field). Closing
    requires a verdict (422 without one) so closed alerts always carry an outcome. Audited, and emits an
    `alerts` SSE event. No bulk endpoint: the UI's bulk actions call it per alert with limited concurrency and
    show per-alert results.
14. **`GET /dashboard?range=1h|24h|7d`** (default `24h`, so the current response is unchanged): adds
    time-bucketed alert counts by severity and execution counts by status, the previous period's figures for
    deltas, and top-5 rules, sources and hosts. No 30d: retention is 7 days.
15. **`GET /assignees`** (callers with `alerts:write` or `cases:write`): distinct assignees already used on
    cases and alerts plus user principals, for the picker. Needed because OIDC users are not principals and
    analysts cannot read `/principals`.
16. **Alert status rules:**
    * Linking an alert to a case (`POST /alerts/{id}/case`) moves it from `new`/`acknowledged` to
      `in_progress`.
    * Closing a case closes its linked open alerts and copies the case verdict onto them
      (`undetermined` if the case has none), so alerts in closed cases do not sit open in the queue forever.
    * The processor creates alerts with `status: "new"`; older alerts with no status count as `new` (in filters
      too).
    * A closed alert that recurs inside its dedup window stays closed (count and `last_seen` update). The next
      window creates a fresh `new` alert anyway, so the processor's dedup logic does not change.

### Frontend

17. **Stack:** keep React 18.3, Vite 5, Tailwind 3.4, TS 5.6 (upgrading means migration churn with no UX
    gain). Add `recharts`, `cmdk`, `sonner`, `@tanstack/react-virtual`, Radix dropdown/popover/tooltip/select/
    scroll-area, and `@fontsource-variable` Inter + JetBrains Mono.
18. **Visual direction:** dark theme by default plus a light theme; calm and precise, like Linear and Vercel.
    Graphite neutrals and one accent. One fixed severity scale everywhere (5 critical, 4 high, 3 medium,
    2 low, 1 info). Dense, virtualized tables. Split-pane triage (list + detail drawer). ⌘K command palette;
    shortcuts (`j/k`, `g a` to Alerts, `/` to search). Live rows highlight briefly when updated. Built for
    desktop (1280 px and up), usable on tablets. WCAG AA. Branding: "SOAR" only.
19. **Pages:** all 10 get the new design. Deepest work: Dashboard, Alerts (fast triage, bulk actions, alert to
    case), Cases (timeline, observables, tasks, SLA), Executions (live run view on the canvas, step
    inputs/outputs), Workflow editor (canvas <-> YAML, inline validation). Sources, Connections, Approvals and
    Admin get the design system with simpler layouts.
20. **Dashboard:** range selector 1h/24h/7d; KPI strip with deltas (alerts, open cases, MTTR, automation rate,
    pending approvals, failed executions); alert volume stacked by severity; execution outcomes over time;
    top-5 rules, sources, hosts; queue health (pending per stream, DLQ); source health (lag, last event,
    errors); latest critical/high alerts; my pending approvals.
21. **Permissions** (from `/me`): pages the caller cannot read are hidden from the nav; actions it cannot take
    stay visible, disabled, with a tooltip like "requires `cases:write`". The server stays the real gate.
22. **Assignee:** "Assign to me" (the `/me` name) plus a picker fed by `GET /assignees`; free text is still
    allowed. The assignee is stored as a display-name string, matching cases today.
23. **Preferences:** theme, density, table columns and saved filters live in localStorage per browser.
    Timestamps: local time with a UTC toggle; relative times show the exact time on hover.
24. **Notifications:** in-app toasts for new critical/high alerts (severity 5/4) and for new approvals the
    caller can decide; can be turned off in preferences. Unread badges on the Alerts and Approvals nav items. No
    browser notification permission prompt, no sound.

### Verification and docs

25. **Verification:**
    * Go tests for UI serving, `/config.json`, the headers and the new endpoints.
    * `tsc` + vitest for the UI.
    * A live run on an isolated `soarx` stack with seeded data (the owner's containers are never touched).
    * Screenshots of every page in both themes, using Playwright and Chromium installed in the scratchpad.
    * No Playwright end-to-end suite in the repo.
26. **Docs:** this plan; a new `docs/ui.md` (build, run, env vars, LAN access, SSH tunnel, security headers);
    README quick start and spec 12/16 updated now that the nginx `web` service is gone.

## Implementation notes (2026-11-16, as built)

Where the build differs from, or adds to, the decisions above:

* **Verdict vocabulary:** alerts use the existing case verdicts `true_positive | false_positive | benign | unknown`
  (not `undetermined`), so a case verdict can be copied onto its alerts unchanged.
* **Embedding:** Vite writes to `internal/webui/dist` (git-ignored). `internal/webui/embed_ui.go` (build tag `ui`)
  embeds it; `embed_noui.go` (default) embeds nothing and serves the "UI not built" page. `make build` = `web-build`
  + `go build -tags ui`; `make build-go` builds without Node.
* **`GET /assignees`** needs `alerts:write` (checked by the route guard); every built-in role that holds
  `cases:write` also holds it.
* **Extra read-only additions** (within decision 6): `q` free-text search on `GET /alerts` and `GET /cases`, and an
  `assignee` filter on `GET /alerts`. Store gained `ListQuery.OrMissing`, `Search` and an `Aggregator` interface
  (`Histogram`, `TopTerms`) implemented for OpenSearch and the memory store.
* **Live alert events:** the processor publishes `ui.alerts` for alerts it creates (severity and title included; one
  summary event when a batch creates more than 20), which drives the toasts and the unread badge.
* **`GET /dashboard`** without `range` is unchanged; the `range` section appears only when `range` is given (the UI
  always sends it). Top rules use the alert title (Wazuh `rule.description`), top hosts `entities.hosts`.
* **Production compose:** the `api` service publishes `${SOAR_API_BIND:-127.0.0.1}:8080` and runs one replica (one
  host port); scale with more api services behind a load balancer.
* **Severity colors** are one red-orange hue stepped by lightness (severity is ordinal), validated in both themes;
  execution outcomes use the status colors.

## Files touched (expected)

* `web/**`: design system, pages, new libraries; `vite.config.ts` output for embedding.
* New Go package for the embedded UI (embed, SPA handler, `/config.json`, headers, placeholder page).
* `internal/api/`: mounts the UI; `PATCH /alerts/{id}`, `/dashboard?range`, `/assignees`; case-close and link
  hooks.
* `internal/processor/`: `status: "new"` on alert creation.
* `internal/domain/`: alert `assignee`. Index mapping in `migrations/` if the alert mapping is strict.
* `internal/config/`: `SOAR_UI_ENABLED`, `SOAR_UI_OIDC_AUTHORITY`, `SOAR_UI_OIDC_CLIENT_ID`.
* `api/openapi.yaml`: the new operations and fields.
* `Makefile`, `deploy/docker/Dockerfile`, `deploy/compose/docker-compose.yml`, `deploy/prod/docker-compose.yml`,
  `deploy/keycloak/realm.json`.
* Deleted: `deploy/web/`, `deploy/docker/web.Dockerfile`.
* `docs/ui.md`, `README.md`, `SOAR_TECHNICAL_SPEC.md` (sections 12, 16).

## Out of scope

* A `soar all` single-process role.
* Cookie sessions; HTTPS on the API (both later security steps).
* A bulk alert endpoint; a 30-day range.
* React 19 / Tailwind 4 / Vite 6 upgrades.
* A Playwright end-to-end suite in the repo; browser notifications.
