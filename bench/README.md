# Headplane benchmark harness

Repeatable, runtime-agnostic measurement for the Bun migration experiment.
The harness runs the **same image shape, configuration, dataset, traffic,
and observation window** against each runtime under test. It does not care
which JS runtime is inside the image: run it once per image (Node image,
Bun image) and compare the two result files.

## What it measures

| Phase    | What                                                                 | Metrics recorded                              |
|----------|----------------------------------------------------------------------|-----------------------------------------------|
| Startup  | Cold container start until `/healthz` answers, repeated N times      | Median start-to-healthy time (ms)             |
| Idle     | Container RSS sampled every 5s after warm-up                         | min / median / p95 / max RSS, sample count    |
| Load     | Fixed-route traffic at fixed concurrency (`bench/workload.mjs`)      | Throughput, error count, p50/p95/p99 latency, RSS min/median/p95/max |
| Shutdown | `docker stop` (SIGTERM, 30s grace)                                   | Stop duration (ms)                            |
| Image    | `docker image inspect`                                               | Unpacked image size (bytes)                   |

Results are written as JSON conforming to [`schema.json`](./schema.json)
into `bench/results/<label>-<timestamp>.json`. Check in the harness and
its instructions — not one-off laptop numbers. CI may record trends, but
the go/no-go run must use the deployment-class architecture and memory
limit.

## Prerequisites

- Docker (the harness shells out to `docker run`, `docker stats`, `docker stop`)
- `python3`, `curl`, `awk`
- A JS runtime (`node` 18+ or `bun`) to execute `bench/workload.mjs`
- A built Headplane image containing the app (see below)

## Quick start

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
6. **Long-lived soak** — run `measure.sh` with a large `HP_IDLE_S` (e.g.
   3600) and inspect RSS growth across samples.
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
