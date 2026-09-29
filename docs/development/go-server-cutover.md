# Go server cutover & rollback guide

How to replace the Node production server with the Go server (`cmd/hp_server`),
and how to roll back. This is the Phase 6 operational companion to the
[RFC](go-server-spa-rfc.md).

## What changes

|                 | Before (Node)                               | After (Go)                                               |
| --------------- | ------------------------------------------- | -------------------------------------------------------- |
| Server process  | `node build/server/index.js`                | `hp_server` (static Go binary, `CGO_ENABLED=0`)          |
| UI delivery     | React SSR per request                       | Static SPA (`build/client`), same React app              |
| Data            | RR loaders/actions server-side              | JSON API at `/admin/api/v1/*` + `/admin/events/live` SSE |
| Image           | `ghcr.io/mandarl/headplane` (`final` stage) | Same repo, `go-final` stage (distroless/static)          |
| Config file     | `config.yaml`                               | Same file, same keys                                     |
| Session cookies | `_hp_auth`                                  | Byte-identical `_hp_auth`                                |
| Database        | `hp_persist.db` (SQLite, drizzle)           | Same file, same schema                                   |

The React UI, xterm.js terminals, and WASM SSH/RDP clients are unchanged.

## Prerequisites

- A `config.yaml` that already works with the Node server. The Go server
  reads the **identical surface**: YAML file (`HEADPLANE_CONFIG_PATH`, default
  `/etc/headplane/config.yaml`), `HEADPLANE_*` env vars (nested keys with
  `__`, e.g. `HEADPLANE_SERVER__PORT=8080`), `*_path` secret files, and
  `${VAR}` interpolation.
- The data directory (`server.data_path`, default `/var/lib/headplane`)
  must be writable: the Go server runs the same drizzle migrations on boot
  and stores `hp_persist.db` there.
- Headscale reachable at `headscale.url` with a valid API key (unchanged).
- If OIDC is configured, the provider must be reachable and the redirect URI
  (`{base_url}/admin/oidc/callback`) unchanged — the flow is identical.

## Compatibility notes (why rollback is safe)

- **Sessions:** the `_hp_auth` cookie scheme is byte-identical (Phase 2
  interop-tested both directions). Cutting over — or rolling back — does
  **not** log anyone out.
- **Database:** same `hp_persist.db`, same schema. The Go server applies the
  same drizzle migration journal on boot; on an existing database this is a
  no-op. No data migration in either direction.
- **Config:** same file. The one behavioral difference: the Go server reads
  the Headscale config file once at startup and never watches it (matches
  `getHeadscaleConfig` semantics); use the restart integration or restart
  the process after editing Headscale's `config.yaml`.

## Building the artifact

```sh
# Full Go+SPA artifact: SPA into build/, Go server into build/hp_server
./build.sh --go

# Docker image (production)
docker build --target go-final -t ghcr.io/mandarl/headplane:go .

# Docker image (debug shell)
docker build --target go-debug -t ghcr.io/mandarl/headplane:go-debug .

# Nix
nix build .#headplane-go   # Go server binary
```

The `go-final` image is `distroless/static` (CA certs included — OIDC and
Headscale TLS work; `scratch` would have needed a manual CA bundle). It
expects the config at `/etc/headplane/config.yaml` (or
`HEADPLANE_CONFIG_PATH`) and the data dir mounted at `/var/lib/headplane`,
exactly like the Node image. `HEADPLANE_LISTEN_FILE=/tmp/headplane-listen`
is set so the bundled `hp_healthcheck` works unchanged.

## Cutover steps (Docker)

1. Build and push the Go image: `docker build --target go-final -t
ghcr.io/mandarl/headplane:<sha>-go . && docker push ...`
2. Stop the Node container. Keep its image tag — that is your rollback.
3. Start the Go container with the **same** config mount, data-dir mount,
   port mapping, and env vars. Nothing else changes.
4. Verify:
   - `curl $BASE/admin/healthz` → `{"status":"OK"}` (500 `ERROR` means
     Headscale is unreachable — same semantics as before).
   - Load `$BASE/admin/` in a browser: the SPA shell renders, existing
     session still logged in (no forced logout).
   - Log out and back in with the Headscale API key; check the machines
     page populates.
   - If OIDC is configured: complete one OIDC login round-trip.
5. Watch the logs for the first 15 minutes (structured `slog` on stderr).

## Rollback (one step)

Redeploy the previous Node image with the same config and data-dir mounts:

```sh
docker run ... ghcr.io/mandarl/headplane:<previous-node-tag>
```

Same config file, same `hp_persist.db`, mutually readable session cookies —
users stay logged in through the rollback. No data migration in either
direction.

## Monitoring / what to watch

- **Memory** is the reason for this project: track the container's RSS
  (idle and p95 under load). The RFC stop rule applies: if the Go server
  does not show a material, sustained RSS reduction on deployment-class
  hardware, do not cut over — the benchmark method is in `bench/`.
- `/admin/healthz`: 200 `OK` vs 500 `ERROR` (Headscale down). Same contract.
- Logs: structured text on stderr (`slog`); look for `component=auth`,
  `component=oidc`, and Headscale API errors after cutover.
- The live SSE stream (`/admin/events/live`): the SPA holds one connection
  per tab; a reconnect storm after deploy is normal and settles.
- `HEADPLANE_LISTEN_FILE`: the Docker healthcheck reads it; if the file is
  missing the container is reported unhealthy even when serving.

## Known gaps and non-goals

- The Go server does not implement SSR: first paint is the SPA shell, data
  fills in via the JSON API. Acceptable for an authenticated admin dashboard
  (no SEO), but it is a visible change on slow connections.
- Upstream divergence: `app/server/**` is no longer the production server.
  Merging future upstream headplane changes becomes manual. This is the
  largest long-term cost (RFC risk 2).
- The drizzle migration journal is applied by the Go server at boot
  (same journal the Node server applied). If the schema ever changes, both
  implementations must be updated in lockstep.
- `hp-agent` (tsnet) remains a separate process, unchanged.
- Benchmarks in `bench/results/` from this phase are **local, directional**
  numbers (no Docker/cgroup limits on the build machine). Final acceptance
  requires re-running `bench/measure.sh` (Docker) or `bench/measure-local.sh`
  on deployment-class hardware (GCE e2-micro class) before cutover.

## Verifying the packaged artifact locally

```sh
./build.sh --go
# test config pointing at a stub or real Headscale:
HEADPLANE_CONFIG_PATH=/tmp/hp-test/config.yaml \
  ./build/hp_server --client-dir build/client &
curl -s http://127.0.0.1:3000/admin/healthz   # {"status":"OK"}
curl -s http://127.0.0.1:3000/admin/ | head -c 200  # SPA shell
# log in: POST form api_key=<key> to /admin/login -> 302 + _hp_auth cookie
curl -s -D - -o /dev/null -X POST --data-urlencode "api_key=<key>" \
  http://127.0.0.1:3000/admin/login | grep -i "set-cookie\|302"
```
