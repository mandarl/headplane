#!/usr/bin/env node
// MARK: Benchmark workload
//
// Fixed-route traffic generator for the Headplane benchmark harness.
// Runtime-agnostic: uses only global fetch and performance.now(), so it
// runs unmodified on Node 18+ and Bun.
//
// Configuration (environment variables):
//   HP_BASE_URL    Base URL of the server under test, including the
//                  basename prefix, e.g. http://127.0.0.1:3000/admin
//   HP_ROUTES      Comma-separated request paths, e.g. "/healthz,/login".
//                  Defaults to "/healthz".
//   HP_CONCURRENCY Number of concurrent in-flight requests. Default 10.
//   HP_DURATION_S  How long to sustain traffic, in seconds. Default 60.
//   HP_COOKIE      Optional `_hp_auth` session cookie value, sent on every
//                  request so authenticated routes can be exercised.
//
// Emits a single JSON object on stdout with request counts, error counts,
// throughput, and latency percentiles (ms).
//
// This file intentionally avoids dependencies: it must run on a bare
// operator machine with nothing but a JS runtime installed.

const baseUrl = (process.env.HP_BASE_URL ?? "http://127.0.0.1:3000/admin").replace(/\/$/, "");
const routes = (process.env.HP_ROUTES ?? "/healthz")
  .split(",")
  .map((r) => r.trim())
  .filter(Boolean);
const concurrency = Math.max(1, Number.parseInt(process.env.HP_CONCURRENCY ?? "10", 10));
const durationMs = Math.max(1, Number.parseFloat(process.env.HP_DURATION_S ?? "60")) * 1000;
// HP_COOKIE: when set, sent as the `_hp_auth` session cookie so the
// workload can exercise authenticated routes on either server.
const cookieHeader = process.env.HP_COOKIE ? { Cookie: `_hp_auth=${process.env.HP_COOKIE}` } : {};

if (routes.length === 0) {
  console.error("workload: HP_ROUTES resolved to an empty route list");
  process.exit(2);
}

const latencies = [];
let completed = 0;
let errors = 0;
const statusCounts = new Map();
const errorTypes = new Map();
// A hung server must not hang the workload: abort requests that take
// longer than this, and count them by error type.
const REQUEST_TIMEOUT_MS = 15000;

async function oneRequest(route) {
  const start = performance.now();
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), REQUEST_TIMEOUT_MS);
  try {
    const res = await fetch(`${baseUrl}${route}`, {
      redirect: "manual",
      headers: cookieHeader,
      signal: ctrl.signal,
    });
    // Drain the body so connection reuse and full transfer cost are measured.
    await res.arrayBuffer();
    const ms = performance.now() - start;
    latencies.push(ms);
    completed += 1;
    statusCounts.set(res.status, (statusCounts.get(res.status) ?? 0) + 1);
  } catch (err) {
    errors += 1;
    const name = err instanceof Error ? err.name : "unknown";
    errorTypes.set(name, (errorTypes.get(name) ?? 0) + 1);
  } finally {
    clearTimeout(timer);
  }
}

async function worker(deadline, routePicker) {
  while (performance.now() < deadline) {
    await oneRequest(routePicker());
  }
}

function percentile(sorted, p) {
  if (sorted.length === 0) return 0;
  const idx = Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1);
  return sorted[Math.max(0, idx)];
}

async function main() {
  let routeIndex = 0;
  const routePicker = () => routes[routeIndex++ % routes.length];
  const deadline = performance.now() + durationMs;
  const startedAt = Date.now();

  await Promise.all(Array.from({ length: concurrency }, () => worker(deadline, routePicker)));

  const elapsedS = (Date.now() - startedAt) / 1000;
  const sorted = [...latencies].sort((a, b) => a - b);
  const summary = {
    routes,
    concurrency,
    duration_s: Number(elapsedS.toFixed(2)),
    requests: completed,
    errors,
    error_types: Object.fromEntries(errorTypes),
    throughput_rps: Number((completed / elapsedS).toFixed(2)),
    status_counts: Object.fromEntries(statusCounts),
    latency_ms: {
      p50: Number(percentile(sorted, 50).toFixed(2)),
      p95: Number(percentile(sorted, 95).toFixed(2)),
      p99: Number(percentile(sorted, 99).toFixed(2)),
      max: Number((sorted[sorted.length - 1] ?? 0).toFixed(2)),
    },
  };
  console.log(JSON.stringify(summary, null, 2));
}

main().catch((err) => {
  console.error(`workload: fatal: ${err instanceof Error ? err.message : String(err)}`);
  process.exit(1);
});
