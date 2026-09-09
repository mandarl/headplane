# Interim Server (Phase 0)

Phase 0 converts the React Router app to an SPA (`ssr: false`) that talks to
a JSON API at `/admin/api/v1/*`. The real Go server implementing that API does
not exist yet (Phases 1–5), so this phase ships a throwaway Node server —
`server/interim.mjs` + `server/interim-stub.mjs` — that serves the SPA, proxies
the server-driven flows back to the existing Node SSR build, and answers the
v1 API with fixed fixtures. It is deleted in Phase 6 when the Go server cuts
over.

## Running

```sh
./build.sh --interim   # builds the SSR proxy server AND the SPA
# terminal 1: the SSR build serving the server-driven flows
node build-ssr/server/index.js &   # listen on INTERIM_SSR_PORT (default 3001)
# terminal 2: the interim server
INTERIM_SSR_PORT=3001 node server/interim.mjs   # listen on 3000
# browse http://127.0.0.1:3000/admin/
```

Environment variables (all optional):

| Variable             | Default        | Purpose                                 |
| -------------------- | -------------- | --------------------------------------- |
| `INTERIM_PORT`       | `3000`         | interim listen port                     |
| `INTERIM_SSR_HOST`   | `127.0.0.1`    | SSR proxy upstream host                 |
| `INTERIM_SSR_PORT`   | `3001`         | SSR proxy upstream port                 |
| `INTERIM_PREFIX`     | `/admin`       | URL prefix, must match `vite.config.ts` |
| `INTERIM_CLIENT_DIR` | `build/client` | SPA static dir served to browsers       |

`build.sh` flags: `--spa` (SPA only), `--ssr-proxy` (SSR server only into
`build-ssr/server`), `--interim` (both; SSR proxy builds first because the
two builds share `build/`).

## Request routing

1. `/admin` → 302 `/admin/` (mirrors `runtime/http.ts`).
2. Server-driven flows reverse-proxy to the SSR build (body, status, and
   headers — including cookies — stream through verbatim):
   - `POST /admin/login` and `POST /admin/login.data` (the SPA's login
     clientAction issues the single-fetch `.data` variant so the action
     result comes back as JSON instead of a re-rendered document),
     any `/admin/logout`
   - `/admin/oidc/start`, `/admin/oidc/callback`
   - `/admin/ssh/*`, `/admin/rdp/*`
   - `/admin/api/info`, `/admin/healthz`, `/admin/events/live`
   - interim utilities still served by Node: `/admin/api/rdp-gateway`,
     `/admin/api/color-scheme`
3. `/admin/api/v1/*` → stub API (`server/interim-stub.mjs`), same contract the
   Go server will implement (mutations, redirects, error bodies, per-action
   behavior). Fixed fixtures; see below.
4. Static files from `build/client` with the production cache semantics:
   immutable `/assets/*`, 1h everything else, traversal → 404.
5. SPA fallback: every other GET under `/admin/` serves `index.html`
   (`Cache-Control: no-cache`) — client routes take over from there.

## Stub sessions

Real auth does not exist yet. All v1 endpoints except the stub session itself
401 (`{"error": "Unauthenticated", "login": {...}}`) until you mint a stub
session:

```sh
curl -X POST -H 'Content-Type: application/json' -d '{"key":"stub"}' \
  -c cookies.txt http://127.0.0.1:3000/admin/api/v1/stub/session
curl -b cookies.txt http://127.0.0.1:3000/admin/api/v1/boot
# DELETE /stub/session clears it
```

The fixture login config reports `oidcEnabled: true` so the login page shows
the SSO button — clicking it navigates (full document) to the proxied
`/admin/oidc/start`, which runs the real OIDC flow against the SSR build.

## Fixtures

Two machines (`node1` online, `server1` tagged + offline), two headscale users
(`alice`, `bob`), one headplane user (stub admin, owner), one pre-auth key per
user class, a sample ACL policy, DNS config, and restrictions. ACL PATCH
rejects any policy containing `SYNTAX_ERROR` with the stub equivalent of the
headscale validation error, so the editor error path is exercisable.

## Limits

- No real headscale connection: every v1 response is canned.
- No session/auth: the stub cookie is a placeholder, not security.
- No persistence: fixture mutations return the right shape but change nothing.
- The file is intentionally dependency-free (no new npm packages).
