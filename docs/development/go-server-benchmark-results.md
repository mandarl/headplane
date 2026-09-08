# Go vs Node benchmark results (local, directional)

Phase 6 benchmark for the [Go server RFC](go-server-spa-rfc.md) stop rule.
These numbers were measured on the build machine with
`bench/measure-local.sh` — **they are directional, not the
deployment-class run**. The RFC stop rule requires re-running on
deployment-class hardware (GCE `e2-micro` class, Docker, cgroup limits)
before production cutover.

- Date: 2026-09-08
- Harness: `bench/measure-local.sh` (server as child process, RSS from
  `/proc/<pid>/status`; no cgroup limits)
- Headscale: `bench/stub-headscale.mjs` (hermetic fake: 3 nodes, 2 users)
- Config: same `config.yaml` for both (same cookie secret, same stub URL,
  same readable Headscale `config_path` so DNS is exercised)
- Durations: 10 cold starts; 30 s warmup + 120 s idle sampling; 90 s load
  at concurrency 10; one held `/admin/events/live` SSE stream during load
- Raw JSON (gitignored): `bench/results/node-local-20260908-141704.json`,
  `bench/results/go-local-20260908-142304.json`

## What each side actually did

The two servers have different architectures, so the workload exercises
the production-equivalent path for each rather than byte-identical work:

- **Node** (`node build/server/index.js` from `main` @ `d48b637`):
  full React SSR per request — `GET /admin/machines`, `/admin/users`,
  `/admin/dns`, `/admin/settings`.
- **Go** (`build/hp_server --client-dir build/client` on
  `rfc/go-server-spa`): the JSON APIs the SPA fetches for the same pages
  — `GET /admin/api/v1/machines`, `/admin/api/v1/users`,
  `/admin/api/v1/dns`, `/admin/api/v1/settings`.

Both runs logged in with the same Headscale API key (`_hp_auth` cookie)
and saw all-200 responses with zero errors.

## Results

| Metric                    | Go           | Node      | Go vs Node     |
| ------------------------- | ------------ | --------- | -------------- |
| Cold start (median of 10) | **23 ms**    | 1149 ms   | **50× faster** |
| Idle RSS (median, 120 s)  | **11.8 MiB** | 143.9 MiB | **12.2× less** |
| Load RSS p50              | **19.4 MiB** | 559.3 MiB | 28.8× less     |
| Load RSS p95              | **19.5 MiB** | 568.9 MiB | **29.2× less** |
| Load RSS max              | 19.7 MiB     | 569.0 MiB | 28.9× less     |
| Throughput                | **3437 rps** | 70.5 rps  | 48.8×          |
| Latency p50               | **1.77 ms**  | 117.24 ms | 66× faster     |
| Latency p95               | **8.15 ms**  | 258.11 ms | 32× faster     |
| Load errors               | 0            | 0         | —              |
| Shutdown (SIGTERM)        | 9 ms         | 58 ms     | —              |

Memory behavior under load is the telling part: Node's RSS climbed from
149.5 MiB to 569.0 MiB over the 90 s window (per-request SSR allocation),
while Go's stayed flat between 12.6 and 19.7 MiB. Idle was rock-flat on
both (11.8 and 143.9 MiB across all 24 samples).

## Stop-rule verdict

**The stop rule's condition is met locally with large margin**: the Go
server shows a material, sustained RSS reduction versus Node — ~12× at
idle and ~29× at load p95 — plus 50× faster cold starts. Memory, the
metric this project exists to optimize, moves decisively in Go's favor.

This is **not** a cutover approval. It is a green light for the
deployment-class validation: re-run `bench/measure.sh` (Docker,
`go-final` vs `final` images, memory-limited) or `bench/measure-local.sh`
on a GCE `e2-micro`-class machine and confirm the gap persists there
before switching production. The local numbers say the gap is large
enough that it should — but the RFC requires the measurement, not the
inference.

## Reproducing

```sh
# stub Headscale (shared by both runs)
STUB_PORT=5001 node bench/stub-headscale.mjs &

# config with a readable Headscale config_path (exercises DNS)
cat > /tmp/bench-config.yaml <<YAML
server:
  host: "127.0.0.1"
  cookie_secret: "<32+ random hex chars>"
  cookie_secure: false
  data_path: "/tmp/bench-data"
headscale:
  url: "http://127.0.0.1:5001"
  config_path: "/tmp/headscale-config.yaml"
YAML

# Node baseline (main checkout, built with ./build.sh --app)
HEADPLANE_CONFIG_PATH=/tmp/bench-config.yaml HEADPLANE_SERVER__PORT=3001 \
  HEADPLANE_SERVER__DATA_PATH=/tmp/bench-data-node \
  HP_LABEL=node HP_START_CMD="node /path/to/main/build/server/index.js" \
  HP_BASE_URL=http://127.0.0.1:3001/admin \
  HP_ROUTES=/machines,/users,/dns,/settings \
  HP_API_KEY=bench-api-key HP_SSE=1 HP_IDLE_S=120 HP_LOAD_S=90 \
  ./bench/measure-local.sh

# Go server (this branch, built with ./build.sh --go)
HEADPLANE_CONFIG_PATH=/tmp/bench-config.yaml HEADPLANE_SERVER__PORT=3000 \
  HEADPLANE_SERVER__DATA_PATH=/tmp/bench-data-go \
  HP_LABEL=go HP_START_CMD="$PWD/build/hp_server --client-dir $PWD/build/client" \
  HP_BASE_URL=http://127.0.0.1:3000/admin \
  HP_ROUTES=/api/v1/machines,/api/v1/users,/api/v1/dns,/api/v1/settings \
  HP_API_KEY=bench-api-key HP_SSE=1 HP_IDLE_S=120 HP_LOAD_S=90 \
  ./bench/measure-local.sh
```
