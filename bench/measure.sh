#!/usr/bin/env bash
# MARK: Benchmark orchestrator
#
# Runs the Headplane Node-vs-Bun benchmark for one container image and
# writes a results file conforming to bench/schema.json.
#
# The harness is runtime-agnostic: the runtime under test is whatever the
# image contains. Run it once per image (Node image, Bun image) with the
# same limits and durations, then compare the two result files.
#
# Required:
#   HP_IMAGE        Container image to benchmark, e.g. headplane:node
#
# Optional:
#   HP_NAME         Label recorded in the results, e.g. "node" (default:
#                   derived from HP_IMAGE)
#   HP_PORT         Container port serving Headplane (default 3000)
#   HP_HOST_PORT    Host port to publish (default: random free port)
#   HP_HEALTH_PATH  Health endpoint path incl. basename (default
#                   /admin/healthz)
#   HP_MEMORY       Container memory limit, e.g. 1g (default 1g)
#   HP_CPUS         Container CPU limit, e.g. 1 (default 1)
#   HP_ENV_FILE     File passed to `docker run --env-file`
#   HP_DOCKER_ARGS  Extra arguments appended to `docker run`
#   HP_STARTUP_RUNS Number of cold-start repetitions (default 10)
#   HP_WARMUP_S     Warm-up sleep before idle sampling (default 30)
#   HP_IDLE_S       Idle sampling window in seconds (default 600)
#   HP_LOAD_S       Load phase duration in seconds (default 120)
#   HP_CONCURRENCY  Load-phase concurrent requests (default 10)
#   HP_ROUTES       Comma-separated load routes (default "/healthz")
#   HP_OUT_DIR      Directory for the results file (default bench/results)
#   HP_RUNTIME_BIN  JS runtime used to execute bench/workload.mjs
#                   ("node" or "bun"; default "node")
#
# Example:
#   HP_IMAGE=headplane:node HP_ENV_FILE=bench/headplane.env.example \
#     HP_IDLE_S=60 HP_LOAD_S=30 ./bench/measure.sh

set -euo pipefail

BENCH_DIR="$(cd "$(dirname "$0")" && pwd)"
: "${HP_IMAGE:?HP_IMAGE is required (container image to benchmark)}"
HP_NAME="${HP_NAME:-$(basename "${HP_IMAGE}" | cut -d: -f2-)}"
HP_PORT="${HP_PORT:-3000}"
HP_HEALTH_PATH="${HP_HEALTH_PATH:-/admin/healthz}"
HP_MEMORY="${HP_MEMORY:-1g}"
HP_CPUS="${HP_CPUS:-1}"
HP_STARTUP_RUNS="${HP_STARTUP_RUNS:-10}"
HP_WARMUP_S="${HP_WARMUP_S:-30}"
HP_IDLE_S="${HP_IDLE_S:-600}"
HP_LOAD_S="${HP_LOAD_S:-120}"
HP_CONCURRENCY="${HP_CONCURRENCY:-10}"
HP_ROUTES="${HP_ROUTES:-/healthz}"
HP_OUT_DIR="${HP_OUT_DIR:-${BENCH_DIR}/results}"
HP_RUNTIME_BIN="${HP_RUNTIME_BIN:-node}"

command -v docker >/dev/null || { echo "measure: docker is required" >&2; exit 2; }
command -v python3 >/dev/null || { echo "measure: python3 is required" >&2; exit 2; }

if [ -z "${HP_HOST_PORT:-}" ]; then
  HP_HOST_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
fi
BASE_URL="http://127.0.0.1:${HP_HOST_PORT}"
HEALTH_URL="${BASE_URL}${HP_HEALTH_PATH}"
CONTAINER="hp-bench-${HP_NAME}-$$"
STATS_FILE="$(mktemp)"
STARTUP_FILE="$(mktemp)"
LOAD_FILE="$(mktemp)"

cleanup() {
  docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
  rm -f "${STATS_FILE}" "${STARTUP_FILE}" "${LOAD_FILE}"
}
trap cleanup EXIT

# Convert a docker MemUsage value ("45.21MiB", "1.2GiB", "512KiB") to bytes.
to_bytes() {
  awk -v v="$1" 'BEGIN {
    n = v + 0;
    if (v ~ /KiB$/) n *= 1024;
    else if (v ~ /MiB$/) n *= 1024*1024;
    else if (v ~ /GiB$/) n *= 1024*1024*1024;
    printf "%.0f", n;
  }'
}

mem_bytes() {
  local usage
  usage="$(docker stats --no-stream --format '{{.MemUsage}}' "${CONTAINER}" 2>/dev/null | head -1 | cut -d/ -f1 | tr -d ' ')"
  to_bytes "${usage}"
}

run_args=(
  -d --rm --name "${CONTAINER}"
  --memory "${HP_MEMORY}" --cpus "${HP_CPUS}"
  -p "127.0.0.1:${HP_HOST_PORT}:${HP_PORT}"
)
if [ -n "${HP_ENV_FILE:-}" ]; then
  run_args+=(--env-file "${HP_ENV_FILE}")
fi
# shellcheck disable=SC2206
if [ -n "${HP_DOCKER_ARGS:-}" ]; then
  run_args+=(${HP_DOCKER_ARGS})
fi
run_args+=("${HP_IMAGE}")

start_container() {
  docker run "${run_args[@]}" >/dev/null
}

# Wait until the health endpoint answers (any 2xx/5xx from the app counts
# as "the server pipeline is up"; headscale itself may be unreachable).
wait_healthy() {
  local deadline=$((SECONDS + 90))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    if curl -fsS -o /dev/null --max-time 2 "${HEALTH_URL}" 2>/dev/null; then
      return 0
    fi
    # Accept a 500 too: healthz reports ERROR when headscale is down, but
    # the server itself is demonstrably serving traffic.
    local code
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "${HEALTH_URL}" 2>/dev/null || true)"
    if [ "${code}" = "500" ]; then
      return 0
    fi
    sleep 1
  done
  echo "measure: timed out waiting for ${HEALTH_URL}" >&2
  docker logs "${CONTAINER}" 2>&1 | tail -20 >&2 || true
  return 1
}

sample_mem() {
  # $1 = output file, $2 = duration seconds
  local out="$1"
  local duration="$2"
  local end=$((SECONDS + duration))
  while [ "${SECONDS}" -lt "${end}" ]; do
    mem_bytes >>"${out}" || true
    sleep 5
  done
}

echo "==> [${HP_NAME}] startup: ${HP_STARTUP_RUNS} cold-start runs"
for _ in $(seq 1 "${HP_STARTUP_RUNS}"); do
  t0="$(date +%s%3N)"
  start_container
  wait_healthy
  t1="$(date +%s%3N)"
  echo "$((t1 - t0))" >>"${STARTUP_FILE}"
  docker rm -f "${CONTAINER}" >/dev/null
done

echo "==> [${HP_NAME}] steady run: warmup ${HP_WARMUP_S}s, idle ${HP_IDLE_S}s, load ${HP_LOAD_S}s"
start_container
wait_healthy
sleep "${HP_WARMUP_S}"

IDLE_FILE="$(mktemp)"
sample_mem "${IDLE_FILE}" "${HP_IDLE_S}" &
IDLE_PID=$!
wait "${IDLE_PID}"

LOAD_STATS="$(mktemp)"
sample_mem "${LOAD_STATS}" "${HP_LOAD_S}" &
LOAD_PID=$!
HP_BASE_URL="${BASE_URL}/admin" \
HP_ROUTES="${HP_ROUTES}" \
HP_CONCURRENCY="${HP_CONCURRENCY}" \
HP_DURATION_S="${HP_LOAD_S}" \
  "${HP_RUNTIME_BIN}" "${BENCH_DIR}/workload.mjs" >"${LOAD_FILE}"
wait "${LOAD_PID}"

echo "==> [${HP_NAME}] shutdown timing (SIGTERM via docker stop)"
t0="$(date +%s%3N)"
docker stop -t 30 "${CONTAINER}" >/dev/null
t1="$(date +%s%3N)"
STOP_MS="$((t1 - t0))"

IMAGE_BYTES="$(docker image inspect "${HP_IMAGE}" --format '{{.Size}}')"

mkdir -p "${HP_OUT_DIR}"
OUT="${HP_OUT_DIR}/${HP_NAME}-$(date +%Y%m%dT%H%M%S).json"
GIT_SHA="$(git -C "${BENCH_DIR}" rev-parse --short HEAD 2>/dev/null || echo unknown)"
ARCH="$(docker image inspect "${HP_IMAGE}" --format '{{.Architecture}}')"

STARTUP_JSON="$(tr '\n' ',' <"${STARTUP_FILE}" | sed 's/,$//')"
IDLE_JSON="$(tr '\n' ',' <"${IDLE_FILE}" | sed 's/,$//')"
LOAD_STATS_JSON="$(tr '\n' ',' <"${LOAD_STATS}" | sed 's/,$//')"
LOAD_JSON="$(cat "${LOAD_FILE}")"
rm -f "${IDLE_FILE}" "${LOAD_STATS}"

python3 - "${OUT}" <<EOF
import json, statistics, sys
out = sys.argv[1]
startup = [${STARTUP_JSON}]
idle = [${IDLE_JSON}]
load_rss = [${LOAD_STATS_JSON}]
load = ${LOAD_JSON}
def pct(xs, p):
    if not xs: return 0
    s = sorted(xs); i = min(len(s)-1, max(0, int(p/100*len(s))))
    return s[i]
result = {
  "schema": "headplane-bench/v1",
  "meta": {
    "label": "${HP_NAME}",
    "image": "${HP_IMAGE}",
    "arch": "${ARCH}",
    "git_sha": "${GIT_SHA}",
    "memory_limit": "${HP_MEMORY}",
    "cpus": "${HP_CPUS}",
  },
  "config": {
    "startup_runs": ${HP_STARTUP_RUNS},
    "warmup_s": ${HP_WARMUP_S},
    "idle_s": ${HP_IDLE_S},
    "load_s": ${HP_LOAD_S},
    "concurrency": ${HP_CONCURRENCY},
    "routes": "${HP_ROUTES}",
  },
  "image": {"size_bytes": ${IMAGE_BYTES}},
  "startup": {
    "runs_ms": startup,
    "median_ms": statistics.median(startup) if startup else 0,
  },
  "idle": {
    "rss_bytes": {
      "min": min(idle) if idle else 0,
      "median": statistics.median(idle) if idle else 0,
      "p95": pct(idle, 95),
      "max": max(idle) if idle else 0,
      "samples": len(idle),
    }
  },
  "load": {
    "requests": load["requests"],
    "errors": load["errors"],
    "throughput_rps": load["throughput_rps"],
    "status_counts": load["status_counts"],
    "latency_ms": load["latency_ms"],
    "rss_bytes": {
      "min": min(load_rss) if load_rss else 0,
      "median": statistics.median(load_rss) if load_rss else 0,
      "p95": pct(load_rss, 95),
      "max": max(load_rss) if load_rss else 0,
      "samples": len(load_rss),
    },
  },
  "shutdown": {"stop_ms": ${STOP_MS}},
}
with open(out, "w") as f:
    json.dump(result, f, indent=2)
print(f"wrote {out}")
EOF

echo "==> [${HP_NAME}] done: ${OUT}"
