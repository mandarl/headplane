#!/usr/bin/env node
// MARK: Benchmark login helper
//
// POSTs an API-key login form to the server under test and prints the
// `_hp_auth` session cookie value on stdout. Both the Node and Go servers
// accept `POST {base}/login` with an `api_key` form field and answer 302
// with a `Set-Cookie: _hp_auth=...` header on success.
//
//   HP_BASE_URL  e.g. http://127.0.0.1:3000/admin
//   HP_API_KEY   the Headscale API key to log in with

const baseUrl = (process.env.HP_BASE_URL ?? "http://127.0.0.1:3000/admin").replace(/\/$/, "");
const apiKey = process.env.HP_API_KEY ?? "";

const res = await fetch(`${baseUrl}/login`, {
  method: "POST",
  headers: { "Content-Type": "application/x-www-form-urlencoded" },
  body: new URLSearchParams({ api_key: apiKey }),
  redirect: "manual",
});

const setCookie = res.headers.get("set-cookie") ?? "";
const match = setCookie.match(/_hp_auth=([^;]+)/);
if (!match) {
  console.error(`login: no _hp_auth cookie (status ${res.status})`);
  process.exit(1);
}
console.log(match[1]);
