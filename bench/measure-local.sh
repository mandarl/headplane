#!/usr/bin/env bash
# MARK: Local (no-Docker) benchmark orchestrator
#
# Measures one Headplane server process through the same phases as
# bench/measure.sh (startup, idle, load, shutdown) but without Docker:
# the server is started as a child process and RSS is sampled from
# /proc/<pid>/status. Run once per server (Go, Node) with identical
# durations and compare the result files.
#
# This is a *directional* measurement, not the deployment-class run the
# RFC's stop rule requires: there are no cgroup memory/CPU limits here,
# and this machine is far beefier than the e2-micro target. The
# deployment-class run is bench/measure.sh against container images.
#
# Required:
#   HP_LABEL      Label recorded in the results, e.g. "go" or "node"
#   HP_START_CMD  Command that starts the server in the foreground,
#                 e.g. "/path/to/hp_server" or "node /path/to/build/server/index.js"
#   HP_BASE_URL   Base URL including the basename prefix,
#                 e.g. http://127.0.0.1:3000/admin
#
# Optional:
#   HP_ENV_PREFIX   Prefix for a per-run env file sourcing (see below)
#   HP_HEALTH_PATH  Health endpoint path relative to HP_BASE_URL
#                   (default /healthz)
#   HP_ROUTES       Comma-separated load routes relative to HP_BASE_URL
#                   (default "/healthz")
#   HP_API_KEY      Headscale API key used to log in for the load phase.
#                   When set, bench/login.mjs obtains a session cookie and
#                   the workload sends it; when unset, routes are hit
#                   unauthenticated.
#   HP_SSE          "1" to hold one /events/live stream open during load
#                   (default 0)
#   HP_STARTUP_RUNS Number of cold-start repetitions (default 10)
#   HP_WARMUP_S     Warm-up sleep before idle sampling (default 30)
#   HP_IDLE_S       Idle sampling window in seconds (default 60)
#   HP_LOAD_S       Load phase duration in seconds (default 60)
#   HP_CONCURRENCY  Load-phase concurrent requests (default 10)
#   HP_OUT_DIR      Directory for the results file (default bench/results)
#   HP_RUNTIME_BIN  JS runtime for the harness scripts (default "node")
#
# Extra environment for the server under test can be exported before
# invoking this script; it is inherited by HP_START_CMD.
#
# Example:
#   HP_LABEL=go HP_START_CMD="$PWD/build/hp_server" \
#     HP_BASE_URL=http://127.0.0.1:3000/admin \
#     HP_ROUTES=/api/v1/machines,/api/v1/users,/api/v1/dns,/healthz \
#     HP_API_KEY=secret HP_SSE=1 \
#     HP_IDLE_S=60 HP_LOAD_S=60 ./bench/measure-local.sh

set -euo pipefail

BENCH_DIR="$(cd "$(dirname "$0")" && pwd)"
: "${HP_LABEL:?HP_LABEL is required}"
: "${HP_START_CMD:?HP_START_CMD is required}"
: "${HP_BASE_URL:?HP_BASE_URL is required (including the /admin prefix)}"
HP_HEALTH_PATH="${HP_HEALTH_PATH:-/healthz}"
HP_ROUTES="${HP_ROUTES:-/healthz}"
HP_STARTUP_RUNS="${HP_STARTUP_RUNS:-10}"
HP_WARMUP_S="${HP_WARMUP_S:-30}"
HP_IDLE_S="${HP_IDLE_S:-60}"
HP_LOAD_S="${HP_LOAD_S:-60}"
HP_CONCURRENCY="${HP_CONCURRENCY:-10}"
HP_OUT_DIR="${HP_OUT_DIR:-${BENCH_DIR}/results}"
HP_RUNTIME_BIN="${HP_RUNTIME_BIN:-node}"
HP_SSE="${HP_SSE:-0}"
# The results-writer (python) reads these from the environment.
export HP_LABEL HP_STARTUP_RUNS HP_WARMUP_S HP_IDLE_S HP_LOAD_S HP_CONCURRENCY HP_ROUTES

HEALTH_URL="${HP_BASE_URL}${HP_HEALTH_PATH}"
STATS_FILE="$(mktemp)"
STARTUP_FILE="$(mktemp)"
LOAD_FILE="$(mktemp)"
SERVER_PID=""

cleanup() {
  if [ -n "${SERVER_PID}" ] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    kill -TERM "${SERVER_PID}" 2>/dev/null || true
    sleep 1
    kill -KILL "${SERVER_PID}" 2>/dev/null || true
  fi
  rm -f "${STATS_FILE}" "${STARTUP_FILE}" "${LOAD_FILE}"
}
trap cleanup EXIT

rss_bytes() {
  local pid="$1" rss_kb
  rss_kb="$(awk '/^VmRSS:/{print $2}' "/proc/${pid}/status" 2>/dev/null || true)"
  if [ -z "${rss_kb}" ]; then echo 0; else echo $((rss_kb * 1024)); fi
}

start_server() {
  # shellcheck disable=SC2086
  ${HP_START_CMD} >/tmp/bench-server-"${HP_LABEL}".log 2>&1 &
  SERVER_PID=$!
}

stop_server() {
  if [ -n "${SERVER_PID}" ] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    kill -TERM "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
  SERVER_PID=""
}

wait_healthy() {
  local deadline=$((SECONDS + 90))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    if curl -fsS -o /dev/null --max-time 2 "${HEALTH_URL}" 2>/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  echo "measure-local: server did not become healthy: ${HEALTH_URL}" >&2
  tail -20 /tmp/bench-server-"${HP_LABEL}".log >&2 || true
  return 1
}

echo "==> [${HP_LABEL}] startup phase (${HP_STARTUP_RUNS} runs)"
: > "${STARTUP_FILE}"
for ((i = 1; i <= HP_STARTUP_RUNS; i++)); do
  start_server
  t0=$(date +%s%N)
  if wait_healthy; then
    t1=$(date +%s%N)
    echo $(((t1 - t0) / 1000000)) >> "${STARTUP_FILE}"
  else
    stop_server
    echo "measure-local: startup run $i failed" >&2
    exit 1
  fi
  stop_server
done

echo "==> [${HP_LABEL}] idle phase (warmup ${HP_WARMUP_S}s, sample ${HP_IDLE_S}s)"
start_server
wait_healthy
sleep "${HP_WARMUP_S}"
: > "${STATS_FILE}.idle"
end=$((SECONDS + HP_IDLE_S))
while [ "${SECONDS}" -lt "${end}" ]; do
  rss_bytes "${SERVER_PID}" >> "${STATS_FILE}.idle"
  sleep 5
done

echo "==> [${HP_LABEL}] load phase (${HP_LOAD_S}s, concurrency ${HP_CONCURRENCY})"
export HP_BASE_URL HP_ROUTES HP_CONCURRENCY
export HP_DURATION_S="${HP_LOAD_S}"
export HP_RUNTIME_BIN
COOKIE=""
if [ -n "${HP_API_KEY:-}" ]; then
  COOKIE="$("${HP_RUNTIME_BIN}" "${BENCH_DIR}/login.mjs")"
  echo "    logged in, cookie length ${#COOKIE}"
fi
export HP_COOKIE="${COOKIE}"

SSE_PID=""
if [ "${HP_SSE}" = "1" ]; then
  "${HP_RUNTIME_BIN}" "${BENCH_DIR}/sse-hold.mjs" >/dev/null 2>&1 &
  SSE_PID=$!
  sleep 2
fi

: > "${STATS_FILE}.load"
"${HP_RUNTIME_BIN}" "${BENCH_DIR}/workload.mjs" > "${LOAD_FILE}" 2>/tmp/bench-workload-"${HP_LABEL}".log &
WORKLOAD_PID=$!
end=$((SECONDS + HP_LOAD_S + 15))
while kill -0 "${WORKLOAD_PID}" 2>/dev/null && [ "${SECONDS}" -lt "${end}" ]; do
  rss_bytes "${SERVER_PID}" >> "${STATS_FILE}.load"
  sleep 2
done
wait "${WORKLOAD_PID}" || echo "measure-local: workload exited nonzero" >&2
if [ -n "${SSE_PID}" ]; then kill -KILL "${SSE_PID}" 2>/dev/null || true; fi

echo "==> [${HP_LABEL}] shutdown phase"
t0=$(date +%s%N)
stop_server
t1=$(date +%s%N)
STOP_MS=$(((t1 - t0) / 1000000))

mkdir -p "${HP_OUT_DIR}"
OUT="${HP_OUT_DIR}/${HP_LABEL}-local-$(date +%Y%m%d-%H%M%S).json"
export GIT_SHA STOP_MS
GIT_SHA="$(git -C "${BENCH_DIR}/.." rev-parse --short HEAD 2>/dev/null || echo unknown)"

python3 - "${STARTUP_FILE}" "${STATS_FILE}.idle" "${STATS_FILE}.load" "${LOAD_FILE}" "${OUT}" <<'PYEOF'
import json, sys, os, math, platform

def stats(path):
    vals = [int(l.strip()) for l in open(path) if l.strip().isdigit() and int(l.strip()) > 0]
    if not vals:
        return {"min": 0, "median": 0, "p95": 0, "max": 0, "samples": 0}
    s = sorted(vals)
    def pct(p):
        i = min(len(s) - 1, max(0, math.ceil(p / 100 * len(s)) - 1))
        return s[i]
    return {"min": s[0], "median": pct(50), "p95": pct(95), "max": s[-1], "samples": len(s)}

startup_ms = [float(l.strip()) for l in open(sys.argv[1]) if l.strip()]
startup_ms.sort()
def pctf(p):
    i = min(len(startup_ms) - 1, max(0, math.ceil(p / 100 * len(startup_ms)) - 1))
    return startup_ms[i]

load = json.load(open(sys.argv[4]))
out = {
    "schema": "headplane-bench/v1",
    "meta": {
        "label": os.environ["HP_LABEL"],
        "image": "local-process",
        "arch": platform.machine(),
        "git_sha": os.environ.get("GIT_SHA", "unknown"),
        "memory_limit": "none (no cgroup limits; directional only)",
        "cpus": str(os.cpu_count()),
        "method": "measure-local.sh (no Docker): server as child process, RSS from /proc",
    },
    "config": {
        "startup_runs": int(os.environ["HP_STARTUP_RUNS"]),
        "warmup_s": int(os.environ["HP_WARMUP_S"]),
        "idle_s": int(os.environ["HP_IDLE_S"]),
        "load_s": int(os.environ["HP_LOAD_S"]),
        "concurrency": int(os.environ["HP_CONCURRENCY"]),
        "routes": os.environ["HP_ROUTES"],
    },
    "startup": {"runs_ms": startup_ms, "median_ms": pctf(50)},
    "idle": {"rss_bytes": stats(sys.argv[2])},
    "load": {
        "requests": load["requests"],
        "errors": load["errors"],
        "error_types": load.get("error_types", {}),
        "throughput_rps": load["throughput_rps"],
        "status_counts": load["status_counts"],
        "latency_ms": load["latency_ms"],
        "rss_bytes": stats(sys.argv[3]),
    },
    "shutdown": {"stop_ms": int(os.environ["STOP_MS"])},
}
json.dump(out, open(sys.argv[5], "w"), indent=2)
print("wrote", sys.argv[5])
PYEOF

echo "==> [${HP_LABEL}] done: ${OUT}"
