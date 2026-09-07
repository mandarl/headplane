# RFC: Replace the Node server with a Go server; convert the React app from SSR to SPA

**Status:** Draft — design only. No implementation in this proposal.
**Author:** Astro (agent) for mandarl
**Date:** 2026-09-07

## 1. Problem statement

Headplane runs on a very constrained GCP VM (~970 MB RAM shared with other
services). The production Node 24 server idles at ~70 MiB RSS and peaks at
~141 MiB under load (measured with `bench/measure.sh` on a GCE e2-micro).

The Bun migration (PR #2, branch `feature/bun-migration`) was evaluated and
**rejected on measured data**: Bun 1.4.1 showed **+31% idle RSS and +55% load
p95 RSS vs Node 24**, plus 7.6× slower cold starts. Bun won on throughput and
latency, but lost on memory — the metric that motivated the migration.
Correctness was never the issue (30/30 runtime contracts passed under Bun
with zero application changes). The migration is off the table unless a later
Bun release closes the idle-RSS gap.

This RFC scopes the remaining architectural lever: the per-request cost is
dominated by React SSR rendering on Node. The proposal is to keep the entire
React UI, ship it as a client-side SPA, and replace the Node server with a
small Go server that serves static files plus a JSON API.

## 2. Goals and non-goals

**Goals**

- Materially reduce idle and peak RSS of the headplane server process on the
  constrained VM, measured with the existing `bench/` harness.
- Preserve the full UI and all user-facing behavior: every page, action,
  OIDC login, API-key login, and the SSH/RDP terminals.
- Preserve the config surface (`config.yaml`, `HEADPLANE_*` env vars,
  `*_path` secrets) and the SQLite database file format, so existing
  deployments upgrade without migration steps and rollback is a redeploy.

**Non-goals (explicit)**

- **Not a UI rewrite.** React, React Router, xterm.js, and the WASM
  SSH/RDP clients stay exactly as they are. The browser still runs
  JavaScript; only the server runtime changes.
- Not a Headscale API change, not a config-schema change, not a database
  schema change.
- Not a pnpm/bun/toolchain discussion — the frontend build stays on the
  current toolchain; only the server artifact changes from
  `build/server/index.js` to a Go binary.

## 3. Current architecture (recap)

- **Runtime:** Node 24. Production entry `app/server/main.ts` → built to
  `build/server/index.js`. A custom HTTP layer (`runtime/http.ts`) provides
  HTTP/1.1 + HTTPS, static-file serving with streaming and cache headers,
  exact-basename 302 (`/admin` → `/admin/`), the Docker listen-file contract
  (`HEADPLANE_LISTEN_FILE` gets `{scheme}://127.0.0.1:{port}{basename}/healthz\n`),
  and graceful SIGTERM shutdown (5 s force-exit guard).
- **Rendering:** React Router 7 SSR via `createRequestListener`
  (`app/server/app.ts`) and `renderToPipeableStream`
  (`app/entry.server.tsx`). Every page load executes React on the server.
- **Sessions:** Server-side sessions in SQLite (`auth_sessions` table), with
  a bespoke signed cookie `_hp_auth` =
  `base64url(JSON).base64url(HMAC-SHA256(cookie_secret, ...))`
  (`app/server/web/auth.ts`). `cookie_secret` must be exactly 32 chars.
  API-key sessions embed the raw Headscale API key in the signed payload and
  bypass all role checks; OIDC sessions resolve roles from the `users` table.
  A 15-minute interval prunes expired sessions.
- **OIDC:** Hand-rolled provider (`app/server/oidc/provider.ts`; `jose` is
  used only for JWT verification). PKCE flow with an unsigned
  `__oidc_state` cookie (30 min, path `/admin/oidc/callback`), plus
  behavioral quirks a port must replicate: `use_pkce` defaults to **false**,
  token endpoint tries `client_secret_post` then falls back to
  `client_secret_basic` (cached), 60 s clock tolerance, subject-claim
  fallback order, gravatar URL formula.
- **Persistence:** `drizzle-orm/node-sqlite`, file
  `<data_path>/hp_persist.db`. Four live tables: `host_info`, `users`,
  `auth_sessions`, `service_description_overrides`. Migrations in `drizzle/`.
  All DB access funnels through `app/server/context.ts` into `AuthService`
  (`app/server/web/auth.ts`), `service-overrides.ts`, and `hp-agent.ts`.
- **Headscale access:** A per-request API client (`apiForRequest`) using the
  session's key (API-key logins) or the configured `headscale.api_key`
  (OIDC logins); `hsLive` keeps a cached live store with versioning that
  feeds the SSE stream at `/events/live`.
- **Existing Go:** The repo already builds Go (`go 1.25.1`, module
  `github.com/tale/headplane`, `CGO_ENABLED=0`): `cmd/hp_agent`,
  `cmd/hp_healthcheck`, `cmd/hp_ssh` / `cmd/hp_rdp` (WASM),
  `cmd/ws_bridge`, `cmd/fake_sh`, plus `internal/config`, `internal/util`.

## 4. Target architecture

```
Browser (unchanged React SPA bundle, static files)
   │  fetch {credentials:"include"}  /  SSE  /  redirects
   ▼
Go server (cmd/hp_server, single static binary)
 ├─ static file server  → build/client  (immutable /assets/*, 1h others,
 │   HEAD semantics, traversal rejection, /admin → /admin/ 302)
 ├─ session middleware  → _hp_auth cookie (byte-identical HMAC scheme),
 │   auth_sessions/users lookup in hp_persist.db
 ├─ /login, /logout, /oidc/start, /oidc/callback  (OIDC via go-oidc + quirks)
 ├─ JSON API  → Headscale (per-request key resolution, live cache + SSE)
 ├─ /healthz, /api/info (bearer secret), /api/color-scheme
 ├─ rdp-gateway webhook client, agent trigger, service overrides,
 │   DNS/config-file patching, docker/k8s/proc integrations
 ├─ TLS (cert/key from config), listen-file contract, SIGTERM shutdown
 └─ config: config.yaml + HEADPLANE_* env + *_path secrets (same surface)
```

The SPA is served from the same origin and basename, so no CORS is needed.
The Go server reads and writes the **same** `hp_persist.db` with the **same**
schema — sessions issued by the Node server must validate on the Go server
and vice versa. This is what makes rollback a plain redeploy.

## 5. Endpoint inventory: current handler → proposed target

“Go endpoint” = new JSON endpoint on the Go server. “Client fetch” = the
SPA calls it with `fetch(..., {credentials:"include"})`. “Stays server” =
must remain a server-driven flow (never exposed to browser JS).

| Method | Path (under `/admin`) | Current handler | Proposed target |
|---|---|---|---|
| GET | `/healthz` | `app/routes/util/healthz.ts` | Go endpoint (same 200/500 semantics) |
| GET | `/events/live` | `app/routes/util/live.ts` (SSE) | Go SSE endpoint; Go owns the live-store subscription |
| GET | `/api/info` | `app/routes/util/info.ts` (bearer `info_secret`) | Go endpoint; secret compared server-side only |
| POST | `/api/color-scheme` | `app/routes/util/color-scheme.ts` | Go endpoint (sets cookie + 302) |
| POST | `/api/rdp-gateway` | `app/routes/rdp-gateway/action.ts` | Go endpoint (webhook client port) |
| POST | `/login` | `app/routes/auth/login/action.ts` (validates raw API key vs Headscale) | **Stays server** — Go endpoint issuing `_hp_auth`; browser never sees the key |
| POST | `/logout` | `app/routes/auth/logout.ts` | **Stays server** — Go endpoint (session delete, optional OIDC end-session redirect) |
| GET | `/oidc/start` | `app/routes/auth/oidc-start.ts` | **Stays server** — Go 302 + `__oidc_state` cookie |
| GET | `/oidc/callback` | `app/routes/auth/oidc-callback.ts` | **Stays server** — Go token exchange + session issuance |
| GET | `/` | `app/routes/home.tsx` loader | Static SPA shell; data via client fetch to Go API |
| GET/POST | `/machines`, `/machines/:id` | `overview.tsx` / `machine.tsx` loaders + `machine-actions.ts` | Client fetch → Go JSON API (Headscale proxy) |
| GET/POST | `/users` | `overview.tsx` loader + `user-actions.ts` | Client fetch → Go JSON API |
| GET/POST | `/acls` | `acl-loader.ts` + `acl-action.ts` | Client fetch → Go JSON API |
| GET/POST | `/dns` | `overview.tsx` loader + `dns-actions.ts` (config-file patch) | Client fetch → Go JSON API (Go needs config-file read/write + `hs.patch` equivalent) |
| GET | `/settings` | `settings/overview.tsx` loader | Client fetch → Go JSON API (config flags) |
| GET/POST | `/settings/auth-keys` | `overview.tsx` loader + `actions.ts` | Client fetch → Go JSON API |
| GET/POST | `/settings/restrictions` | `overview.tsx` loader + `actions.ts` (config patch) | Client fetch → Go JSON API |
| GET/POST | `/settings/agent` | `agent.tsx` loader + action (`triggerSync`) | Client fetch → Go JSON API (Go owns agent subprocess or equivalent) |
| GET | `/ssh/:id` | `ssh/page.tsx` loader (mints 5-min pre-auth key) | **Stays server-shaped** — Go endpoint minting the key; SPA fetches it, never mints |
| GET | `/rdp/:id` | `rdp/page.tsx` loader | Same as SSH |
| GET/HEAD | `/assets/*`, other static | `runtime/http.ts` static handler | Go static file server (same cache headers, HEAD, traversal rules) |

Security boundary, stated plainly: the browser must never hold a Headscale
API key, a client secret, the `info_secret`, or the ability to mint
pre-auth keys. Those stay behind Go endpoints; the per-request key
resolution (`apiForRequest` pattern) moves into the Go server.

## 6. SPA conversion notes

- React Router supports SPA mode (`ssr: false` in `react-router.config.ts`;
  currently `ssr: true`). In SPA mode there is no server build: no
  `entry.server.tsx`, no server loaders/actions. The app ships as the
  static `build/client` bundle.
- Data loaders (machines, users, ACLs, DNS reads, auth keys, agent status)
  become client-side fetches (route `clientLoader`s or `useEffect`) against
  the Go JSON API on the same origin — cookie rides along via
  `credentials: "include"`.
- The OIDC flow stays server-driven (redirects + `__oidc_state` cookie);
  only page chrome becomes client-rendered. Session validation becomes Go
  middleware on every API request.
- **What is lost:** SSR first paint. For an authenticated admin dashboard
  (no SEO, JS required anyway for the terminals), this is an acceptable
  trade — but it should be measured, not assumed.
- `app/routes/util/redirect.ts` is dead code (not in the route tree) and can
  be dropped in the conversion.

## 7. Phased implementation plan (rough effort)

Estimates are for one experienced developer who knows the codebase. This is
**8–14 weeks** of focused work, not a side project.

- **Phase 0 — SPA conversion of the React app (1–2 wks).** Flip
  `ssr: false`; convert data loaders to client fetches against a stub API;
  keep security-sensitive flows (login, OIDC, key minting, `/api/info`)
  hitting the existing Node server. The Node server keeps serving the SPA
  shell in the interim. Exit: the UI runs as an SPA against the Node API.
- **Phase 1 — Go server skeleton (1 wk).** `cmd/hp_server`: config loading
  (yaml + `HEADPLANE_*` env + `_path` secrets + `${VAR}` interpolation),
  static file serving with the exact cache/HEAD/traversal semantics, TLS,
  basename redirect, listen-file contract, `/healthz`, structured logging,
  SIGTERM shutdown with the 5 s guard. Exit: serves the SPA shell; passes
  the `tests/runtime` server-smoke/TLS/lifecycle contracts.
- **Phase 2 — Session/auth parity (1–2 wks).** Byte-identical `_hp_auth`
  cookie scheme, `auth_sessions`/`users` access against the same
  `hp_persist.db`, role/capability bitmask port, API-key vs OIDC principal
  handling, login/logout, 15-min prune. Exit: interop test — a session
  cookie issued by the Node server validates on the Go server and vice
  versa; existing auth unit tests ported to Go.
- **Phase 3 — OIDC port (1–2 wks).** `go-oidc` + `golang.org/x/oauth2`
  replicating every quirk in §3 (PKCE defaults, post→basic fallback,
  60 s clock tolerance, subject-claim order, gravatar formula, unsigned
  `__oidc_state` cookie shape). Exit: login/logout/RP-initiated logout
  verified against the existing dex test harness
  (`tests/integration/oidc/`).
- **Phase 4 — Headscale API layer + live store (2–3 wks, the big one).**
  Port `transport.ts`, the API resource modules, per-request key
  resolution, and the `hsLive` cache with versioning feeding the Go SSE
  endpoint. Exit: all data pages and mutations work; SSE streams.
- **Phase 5 — Remaining endpoints (2 wks).** rdp-gateway webhook, agent
  trigger/sync, service overrides, DNS + restrictions config-file patching
  (Go equivalent of `hs.patch`), docker/k8s/proc integrations (restart
  Headscale on config change), `/api/info`. Exit: feature parity checklist
  green.
- **Phase 6 — Packaging, benchmarks, cutover (1–2 wks).** `build.sh`
  `--server` flag, Dockerfile stage (distroless or `scratch` + CA certs —
  note: scratch needs explicit CA bundle for OIDC/Headscale TLS),
  `nix/package.nix` derivation, `bench/` comparison Node vs Go, soak test,
  canary, docs. Rollback = redeploy the Node image (same DB file, same
  config, compatible cookies).

## 8. Risks

1. **Auth/OIDC parity bugs.** The session cookie and OIDC quirks are
   bespoke; any byte-level divergence locks users out or, worse, weakens
   auth. Mitigation: interop tests (Phase 2) and the dex harness (Phase 3)
   before any cutover.
2. **Upstream fork divergence.** This abandons `app/server/**` — roughly
   half the TypeScript codebase. Merging future upstream headplane changes
   becomes manual and eventually stops being feasible. This is the largest
   long-term cost and the main argument against doing it at all.
3. **Config-file patching semantics.** `hs.patch()` (DNS, restrictions)
   mutates the Headscale YAML with round-trip fidelity; a Go reimplementation
   must preserve comments/formatting or operators will notice.
4. **Live-store/SSE reimplementation.** The caching, versioning, and
   backpressure behavior of `hsLive` is subtle; a naive poll loop will
   either hammer Headscale or serve stale data.
5. **SQLite driver choice.** `mattn/go-sqlite3` needs cgo, which conflicts
   with the repo's `CGO_ENABLED=0` builds; `modernc.org/sqlite` is pure Go
   but slower and heavier to compile. Either way the drizzle migration
   journal needs a Go runner or a freeze-and-hand-maintain strategy.
6. **Estimated payoff may not materialize.** A Go server idling at
   ~15–25 MiB RSS is plausible but unproven for this workload; the
   `bench/` harness must confirm a material win before cutover, with a
   stop rule if it doesn't.

## 9. Acceptance criteria and rollback

- `bench/measure.sh` on deployment-class hardware shows a **material,
  sustained RSS reduction** vs the Node baseline (idle and p95 under matched
  load); numeric threshold to be set from the Node baseline before cutover
  work begins. If the delta is marginal, stop — do not cut over.
- All `tests/runtime` contracts pass against the Go server; auth interop
  tests pass both directions; OIDC verified against dex.
- No latency regression beyond an agreed band; zero correctness regressions.
- **Rollback:** redeploy the previous Node image. Same config file, same
  `hp_persist.db`, mutually readable session cookies — no data migration in
  either direction.

## 10. Open questions for the maintainer

1. Is the upstream-divergence cost (risk 2) acceptable, or does staying
   mergeable with upstream rule out any server rewrite?
2. SQLite driver: `modernc.org/sqlite` (pure Go, fits `CGO_ENABLED=0`) vs
   `mattn/go-sqlite3` (cgo, faster, complicates Docker/Nix)?
3. Should the Go server run the drizzle migration journal, or should
   migrations freeze at the cutover version?
4. Is losing SSR first paint acceptable for the login page specifically
   (the one unauthenticated surface)?
5. What is the minimum memory win that justifies 8–14 weeks of work —
   and is that bar actually reachable, or is a larger VM the rational
   answer?
6. Does the `hp-agent` (tsnet) stay a separate process, or get folded into
   the Go server?

---

*Related: PR #2 (Bun migration, closed — Bun 1.4.1 measured +31% idle /
+55% load p95 RSS vs Node 24 on a GCE e2-micro); the `bench/` harness and
`tests/runtime/` contracts from that branch are the measurement tools for
this RFC's acceptance criteria.*
