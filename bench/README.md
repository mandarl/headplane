# Headplane benchmark harness

Repeatable, runtime-agnostic measurement for comparing the Node and Go
Headplane servers. The harness runs the **same configuration, dataset,
traffic, and observation window** against each server under test. It does
not care which runtime is inside the image or binary: run it once per
target (Node server, Go server) and compare the two result files.

Two runners are provided:

- `bench/measure.sh` — Docker-based: builds (or reuses) a container
  image per target and measures inside cgroup limits. Use this for the
  deployment-class go/no-go decision.
- `bench/measure-local.sh` — local-process: starts a server binary
  directly and measures host RSS via `/proc`. Use this for fast,
  directional iteration; it is **not** a substitute for the
  deployment-class run.

## What it measures

| Phase    | What                                                            | Metrics recorded                                                     |
| -------- | --------------------------------------------------------------- | -------------------------------------------------------------------- |
| Startup  | Cold start until `/admin/healthz` answers, repeated N times     | Median start-to-healthy time (ms)                                    |
| Idle     | Process RSS sampled every 5s after warm-up                      | min / median / p95 / max RSS, sample count                           |
| Load     | Fixed-route traffic at fixed concurrency (`bench/workload.mjs`) | Throughput, error count, p50/p95/p99 latency, RSS min/median/p95/max |
| Shutdown | SIGTERM                                                         | Stop duration (ms)                                                   |
| Image    | `docker image inspect` (Docker runner only)                     | Unpacked image size (bytes)                                          |

Results are written as JSON conforming to [`schema.json`](./schema.json)
into `bench/results/<label>-<timestamp>.json`. Check in the harness and
its instructions — not one-off laptop numbers. CI may record trends, but
the go/no-go run must use the deployment-class architecture and memory
limit.

## Go vs Node: what each target runs

Both targets serve the same SPA client bundle and answer the same
configuration against the same stub Headscale:

- **Node** (`headplane:node` image or `node build/server/index.js`):
  the React Router SSR server. Each benchmark route (`/admin/machines`,
  `/admin/users`, `/admin/dns`, `/admin/settings`) does full
  server-side rendering.
- **Go** (`go-final` image or `./build/hp_server --client-dir
build/client`): the Go API server. The same pages are served as the
  static SPA shell; the benchmark hits the JSON endpoints the SPA
  fetches (`/admin/api/v1/machines`, `/admin/api/v1/users`,
  `/admin/api/v1/dns`, `/admin/api/v1/settings`), plus one held SSE
  stream on `/admin/events/live`.

The route lists differ because the architectures differ (SSR pages vs
SPA JSON APIs); the harness documents each run's route list in its
result file. Memory is the deciding metric: the cutover stop rule is in
[`docs/development/go-server-cutover.md`](../docs/development/go-server-cutover.md).

## Prerequisites (Docker runner)

- Docker (the harness shells out to `docker run`, `docker stats`, `docker stop`)
- `python3`, `curl`, `awk`
- A JS runtime (`node` 18+) to execute `bench/workload.mjs`
- A built Headplane image containing the app (see below)

## Prerequisites (local runner)

- `python3`, `curl`, `awk`, and `node` 18+ (for the workload and the stub Headscale)
- The servers under test, built locally (see below)
- A stub Headscale: `bench/stub-headscale.mjs`

## Quick start (local runner)

The local runner needs a Node baseline checkout. Example with two worktrees:

```sh
# 1. Build the SPA + Go server on this branch:
./build.sh --go

# 2. Build the Node baseline from main (separate checkout):
cd /path/to/headplane-main && ./build.sh --app --skip-pnpm-prune

# 3. Start the stub Headscale (both servers point at it):
STUB_PORT=5001 node bench/stub-headscale.mjs &

# 4. Run the Node baseline:
HP_LABEL=node HP_PORT=3001 \
  HP_START_CMD="node /path/to/headplane-main/build/server/index.js" \
  HP_ROUTES="/admin/machines /admin/users /admin/dns /admin/settings" \
  HP_SSE=1 ./bench/measure-local.sh

# 5. Run the Go server (same config file, separate data dir):
HP_LABEL=go HP_PORT=3000 HP_DATA_DIR=/tmp/bench-data-go \
  HP_START_CMD="/path/to/rfc-branch/build/hp_server --client-dir /path/to/rfc-branch/build/client" \
  HP_ROUTES="/admin/api/v1/machines /admin/api/v1/users /admin/api/v1/dns /admin/api/v1/settings" \
  HP_SSE=1 ./bench/measure-local.sh
```

Both runs use the same `HEADPLANE_CONFIG_PATH` (see
`bench/headplane.env.example`): the same cookie secret, the same stub
Headscale URL, and the same readable Headscale `config_path` so the
DNS route is exercised rather than erroring.

## Quick start (Docker runner)

```sh
# 1. Build the images under test (example tags).
docker build -t headplane:node .
# (Bun image target lands in a later commit; same tag scheme: headplane:bun)

# 2. Prepare runtime config for the container.
cp bench/headplane.env.example bench/headplane.env

# 3. Run the harness once per image with identical limits.
HP_IMAGE=headplane:node HP_NAME=node HP_ENV_FILE=bench/headplane.env \
  HP_MEMORY=1g HP_CPUS=1 ./bench/measure.sh

HP_IMAGE=headplane:bun HP_NAME=bun HP_ENV_FILE=bench/headplane.env \
  HP_MEMORY=1g HP_CPUS=1 ./bench/measure.sh

# 4. Compare bench/results/node-*.json against bench/results/bun-*.json.
```

For a quick smoke of the harness itself, shrink the windows:

```sh
HP_IMAGE=headplane:node HP_NAME=node-smoke HP_ENV_FILE=bench/headplane.env \
  HP_STARTUP_RUNS=2 HP_WARMUP_S=5 HP_IDLE_S=20 HP_LOAD_S=15 \
  ./bench/measure.sh
```

## Configuration reference

All knobs are environment variables; see the header of
[`measure.sh`](./measure.sh) for the full list and defaults. The important
ones for matched comparisons:

- `HP_IMAGE` (required) — image under test.
- `HP_NAME` — label recorded in the results file (`node`, `bun`).
- `HP_MEMORY` / `HP_CPUS` — container limits. **Keep identical across runs.**
  Use values close to the constrained deployment target.
- `HP_IDLE_S` (default 600), `HP_LOAD_S` (default 120),
  `HP_STARTUP_RUNS` (default 10), `HP_CONCURRENCY` (default 10),
  `HP_ROUTES` (default `/healthz`).
- `HP_ENV_FILE` — passed to `docker run --env-file`; this is how the
  container gets its Headplane configuration.
- `HP_RUNTIME_BIN` — `node` or `bun`; which runtime executes the workload
  generator itself (does not affect the server under test).

## How to read the results

The migration hypothesis is: **lower production memory with equivalent
behavior.** Compare the paired result files on:

1. **Idle RSS** (`idle.rss_bytes.median`) — the headline number. Expect the
   planning range from the migration plan; the actual go/no-go threshold is
   set after the Node baseline is recorded.
2. **Load RSS** (`load.rss_bytes.p95` and `max`) — the gap usually narrows
   under sustained application work.
3. **Startup** (`startup.median_ms`) — must not regress.
4. **Latency** (`load.latency_ms.p95`) — must not regress materially.
5. **Shutdown** (`shutdown.stop_ms`) — graceful SIGTERM behavior.
6. **Correctness** — the runtime contract suite (`pnpm run test:runtime`)
   must pass identically on both images.

## Scenario coverage

The migration plan lists eight representative scenarios. The harness covers
them as follows:

1. **Cold start** — the startup phase (median of N runs).
2. **Idle** — the idle phase (10-minute default window).
3. **Read path** — load phase routes (`HP_ROUTES`, e.g.
   `/healthz,/login`); extend the route list as needed.
4. **Write path** — not yet automated (requires an authenticated session
   against an isolated instance); recorded here as a follow-up.
5. **Concurrency** — the load phase at fixed low/moderate concurrency.
6. **Long-lived soak** — run `measure.sh` with a large `HP_IDLE_S` (e.g. 3600) and inspect RSS growth across samples.
7. **Shutdown** — the shutdown phase (SIGTERM while the load phase has
   just completed; in-flight behavior is covered by the lifecycle
   contract test in `tests/runtime`).
8. **Failure** — the harness points at an unreachable Headscale by
   default (`bench/headplane.env.example`), so every run exercises the
   degraded-dependency path; invalid configuration is covered by the
   contract tests.

## Notes and limits

- Memory is sampled from `docker stats` (cgroup RSS), not from inside the
  JS heap: this is the number the constrained VM actually feels.
- `bench/results/` is git-ignored; result files are evidence artifacts,
  not source. Attach the paired files to the go/no-go decision record.
- `bench/headplane.env` (your copy with real secrets) is git-ignored; only
  the `.example` file is checked in.

## Go vs Node (Phase 6)

The same harness compares the Go server against the Node baseline. Two
runners exist:

- **`bench/measure.sh`** (Docker, deployment-class): builds/runs the
  `go-final` vs `final` image targets with cgroup limits. This is the run
  the RFC stop rule requires — it must happen on deployment-class hardware
  (GCE e2-micro class) before cutover.
- **`bench/measure-local.sh`** (no Docker): runs each server as a child
  process and samples RSS from `/proc`. Directional only — no memory/CPU
  limits are applied, and the build machine is far beefier than the
  deployment target. Useful for quick iteration and for CI smoke runs.

Both runners share `bench/workload.mjs` (set `HP_COOKIE` to exercise
authenticated routes) and `bench/stub-headscale.mjs` (a hermetic fake
Headscale so the servers' own memory is measured, not a real Headscale's).

Local example — Go server vs Node baseline (`main` branch build):

```sh
# 1. Build both servers.
/build.sh --go                                    # Go + SPA -> build/
git worktree add /tmp/hp-main main                # Node baseline
(cd /tmp/hp-main && ./build.sh --app)             # -> build/server/index.js

# 2. Start the stub Headscale both servers will talk to.
STUB_PORT=5001 node bench/stub-headscale.mjs &

# 3. Shared bench config (same file for both servers — the config surface
#    is identical). Ports differ per run via HEADPLANE_SERVER__PORT.
cat > /tmp/bench-config.yaml <<YAML
server:
  host: "127.0.0.1"
  cookie_secret: "0123456789abcdef0123456789abcdef"
  cookie_secure: false
  data_path: "/tmp/bench-data"
headscale:
  url: "http://127.0.0.1:5001"
YAML

# 4. Measure the Go server (authenticated JSON API workload + held SSE).
HEADPLANE_CONFIG_PATH=/tmp/bench-config.yaml \
HEADPLANE_SERVER__PORT=3000 \
HP_LABEL=go HP_START_CMD="$PWD/build/hp_server" \
HP_BASE_URL=http://127.0.0.1:3000/admin \
HP_ROUTES=/api/v1/machines,/api/v1/users,/api/v1/dns,/api/v1/settings,/healthz \
HP_API_KEY=bench-api-key HP_SSE=1 \
HP_IDLE_S=60 HP_LOAD_S=60 \
./bench/measure-local.sh

# 5. Measure the Node baseline (authenticated SSR page workload).
HEADPLANE_CONFIG_PATH=/tmp/bench-config.yaml \
HEADPLANE_SERVER__PORT=3001 \
HP_LABEL=node HP_START_CMD="node /tmp/hp-main/build/server/index.js" \
HP_BASE_URL=http://127.0.0.1:3001/admin \
HP_ROUTES=/machines,/users,/dns,/healthz \
HP_API_KEY=bench-api-key \
HP_IDLE_S=60 HP_LOAD_S=60 \
./bench/measure-local.sh

# 6. Compare bench/results/go-local-*.json against node-local-*.json.
```

**Fairness notes (read before quoting numbers):**

- The routes differ because the products differ: the Node baseline serves
  SSR pages (`/admin/machines` renders React on the server per request);
  the Go server serves the JSON API the SPA fetches. This is the genuine
  production workload of each deployment — the per-request work difference
  is part of what the benchmark is measuring, not an artifact to normalize
  away.
- Local runs have no cgroup limits: RSS numbers are directional. The
  go/no-go decision needs `measure.sh` on deployment-class hardware.
- `HP_API_KEY` must match a key the stub accepts (`bench-api-key`);
  against a real Headscale, use a real (revocable) API key.
