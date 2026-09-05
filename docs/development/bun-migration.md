# Bun migration: runtime spike log

Tracks the Bun runtime experiment on `feature/bun-migration`. This file is
the running incompatibility log for the migration: every Bun-only failure,
workaround, and verdict lands here with a date and the exact Bun version
pinned in `mise.toml` and `.github/workflows/bun-experiment.yml`.

Compatibility code rule (from the migration plan): prefer a narrow runtime
adapter at the bootstrap boundary over scattered `if (Bun)` branches
throughout application code.

## Spike 1 — 2026-09-05, Bun 1.4.1

**Method.** The pnpm/Node-built production server (`build/server/index.js`,
from the unchanged build) is executed with the pinned Bun binary via
`HP_TEST_RUNTIME=bun`; the identical runtime-contract suite
(`pnpm run test:runtime`) runs against it in the Bun experiment lane.
Vitest itself still runs under Node — only the spawned server process
changes runtime, which isolates the question "does the emitted server run
under Bun?" from test-runner concerns.

**Result: 30/30 runtime-contract tests pass under Bun 1.4.1 with zero
application changes.**

**What this proves.** The production bootstrap (`app/server/main.ts`), the
custom `runtime/http.ts` listener (HTTP/1.1, HTTPS termination, static-asset
serving, HEAD semantics, cache headers, path-traversal rejection),
`@react-router/node` request handling, basename redirects, `/healthz`,
SIGTERM shutdown, and the listen-file contract all behave identically
under Bun.

**What this does NOT prove.** The SQLite, undici-transport, Docker
discovery, and child-process contracts execute inside the vitest process,
which runs under Node in both lanes. Bun behavior for
`drizzle-orm/node-sqlite`, undici streaming/abort/timeout semantics, and
`node:child_process` is still untested and remains on the critical path
before any production cutover.

## Incompatibility log

| Date       | Bun   | Area | Symptom                     | Root cause | Resolution / status                          |
|------------|-------|------|-----------------------------|------------|----------------------------------------------|
| 2026-09-05 | 1.4.1 | —    | None found in spike 1       | —          | No adapter needed; server runs unmodified    |

## Open risks

- `drizzle-orm/node-sqlite` under Bun: untested. Bun documents
  `node:sqlite` support, but Drizzle ships a separate `bun-sqlite`
  driver — needs a dedicated Bun-side persistence probe before cutover.
- undici under Bun: transport behavior, streaming bodies, aborts,
  timeouts, TLS — untested.
- `node:child_process` / `node:readline` semantics under Bun — untested.
- Native install scripts and architecture-sensitive packages under a
  Bun-based install — not attempted; pnpm remains the installer.
- Running vitest itself under Bun — deferred; Node remains the runner.

## Next steps (plan phases 4–6)

1. Bun container target in the Dockerfile beside the Node target.
2. Matched Node/Bun benchmark and soak using `bench/`.
3. Go/hold/stop decision on measured memory data — Bun must show a
   material, sustained memory reduction with zero correctness regressions.
