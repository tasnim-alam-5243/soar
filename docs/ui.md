# SOAR web console: build, run and access

The React console (`web/`) is compiled into the `soar` binary. The `api` role serves it at `/` and the REST API at
`/api/v1` on the same port (8080 by default), so there is no separate web server, no CORS and no proxy. Design
decisions: [ui-dashboard-plan.md](ui-dashboard-plan.md).

## Build

```bash
make build        # npm ci (first time), Vite build into internal/webui/dist, then go build -tags ui -> bin/soar
bin/soar api      # API + UI on SOAR_HTTP_ADDR (default :8080)
```

* `make build` needs Node 22 and npm. The bundle lands in `internal/webui/dist/` (not in git) and is embedded with
  `go:embed` only when the `ui` build tag is set.
* `make build-go`, a plain `go build ./...` or `go test ./...` need no Node: they embed nothing, and `/` shows a short
  page that says how to build the UI. Go CI therefore never depends on Node.
* `make image-soar` (and `make up`) build the image with a Node stage, so the container also carries the UI.
* UI development with hot reload: `make web-dev` serves on `:3000` and proxies `/api` and `/config.json` to
  `http://127.0.0.1:8080` (set `SOAR_API` to point it elsewhere). `make web-test` type-checks and runs the unit tests.

## Runtime settings (api role)

| Variable | Default | Meaning |
|---|---|---|
| `SOAR_UI_ENABLED` | `true` | `false` serves the API only (`/` answers 404). |
| `SOAR_UI_OIDC_AUTHORITY` | `SOAR_OIDC_ISSUER` | OIDC issuer the browser signs in with. Empty: API-key sign-in only. |
| `SOAR_UI_OIDC_CLIENT_ID` | `soar-web` | Public OIDC client of the UI (authorization code + PKCE). |
| `SOAR_API_PUBLISH` (compose) | `127.0.0.1` | Address the 8080 port is published on. |

The browser reads these from `GET /config.json`, built at request time, so one binary works in every environment.
In Keycloak, the `soar-web` client needs the UI's address in its redirect URIs and web origins. The dev realm
(`deploy/keycloak/realm.json`) lists `http://localhost:8080` and `http://127.0.0.1:8080`, plus `:3000` for
`make web-dev`. An existing Keycloak keeps its old client settings (the realm is imported only on first start), so
add the `:8080` URIs in its admin console.

## Reaching the UI from your workstation

Compose publishes 8080 on `127.0.0.1` only. Two options for the lab:

1. **LAN address, API key.** Set `SOAR_API_PUBLISH=<this host's LAN address>` (for example `10.10.192.198`) in
   `deploy/compose/.env` and `make up`. Browse to `http://<address>:8080` and sign in with an API key. The key crosses
   the network unencrypted, so use this on a trusted network only. Single sign-on is disabled on this page because
   browsers allow PKCE only on HTTPS or `localhost`; the sign-in page says so.
2. **SSH tunnel, single sign-on.** Nothing is exposed and Keycloak login works, because the browser sees `localhost`:

   ```bash
   ssh -L 8080:127.0.0.1:8080 -L 8180:127.0.0.1:8180 <user>@<soar-host>
   # then open http://localhost:8080
   ```

HTTPS on the API (`SOAR_TLS_CERT_FILE` / `SOAR_TLS_KEY_FILE`) is the production answer; production compose binds the
port to `SOAR_API_BIND` and expects a TLS-terminating proxy in front.

## What the server sends

* `index.html` for every non-API path without a file (client-side routes), `Cache-Control: no-cache`.
* `/assets/*` (content-hashed): `Cache-Control: public, max-age=31536000, immutable`. A missing asset is a real 404,
  never `index.html`, so a stale tab cannot load HTML as JavaScript.
* On every UI response: `Content-Security-Policy` with `default-src 'self'` and `script-src 'self'` (no inline
  scripts; inline styles are allowed because Monaco and the toast library inject `<style>` elements), `connect-src`
  and `frame-src` limited to this origin plus the OIDC issuer's origin, `frame-ancestors 'none'`, `object-src 'none'`;
  also `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
  `Cross-Origin-Opener-Policy: same-origin` and a restrictive `Permissions-Policy`.
* Fonts (Inter, JetBrains Mono) and Monaco are bundled: the console makes no request to any CDN and works on closed
  networks.

## Sign-in and session

OIDC (authorization code + PKCE) or an API key. The token is kept in `sessionStorage` of the tab and cleared on
sign-out; live updates (`GET /api/v1/stream`) pass it as `access_token` because `EventSource` cannot send headers.
Pages a role cannot read are hidden; actions it cannot take are disabled with a tooltip naming the permission. The
API remains the only gate.

## Using it

* `⌘K` / `Ctrl K`: command palette (jump to pages, search alerts and cases, open an id like `alt_…`, toggle theme,
  time zone and density). `?` lists every shortcut.
* Alerts: `j`/`k` move, `Enter` opens the side panel, `x` selects, `a` acknowledges, `i` assigns to you, `c` closes with
  a verdict, `o` opens or links a case. Bulk actions apply to the selection one alert at a time and report each
  failure. Saved views keep filter sets in this browser.
* Preferences (theme, density, local time or UTC, notifications) are stored in this browser only.
