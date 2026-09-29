#!/usr/bin/env node
// MARK: Benchmark SSE holder
//
// Holds one server-sent-events stream open for the duration of a load
// phase, mirroring the SPA which keeps `/events/live` connected while
// the user works. Runs until killed.
//
//   HP_BASE_URL  e.g. http://127.0.0.1:3000/admin
//   HP_COOKIE    _hp_auth cookie value

const baseUrl = (process.env.HP_BASE_URL ?? "http://127.0.0.1:3000/admin").replace(/\/$/, "");
const cookie = process.env.HP_COOKIE ?? "";

const res = await fetch(`${baseUrl}/events/live`, {
  headers: cookie ? { Cookie: `_hp_auth=${cookie}` } : {},
});
if (!res.ok || !res.body) {
  console.error(`sse-hold: stream failed (status ${res.status})`);
  process.exit(1);
}
// Drain forever; the harness kills this process when the load phase ends.
const reader = res.body.getReader();
for (;;) {
  const { done } = await reader.read();
  if (done) break;
}
