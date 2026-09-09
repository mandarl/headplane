#!/usr/bin/env node
// MARK: Stub Headscale for benchmarks
//
// A minimal fake Headscale HTTP API that answers the endpoints both the
// Node and Go Headplane servers call, with a small fixed dataset. This
// keeps benchmark runs hermetic: the servers' own memory/CPU is measured,
// not a real Headscale's.
//
//   STUB_PORT   listen port (default 5001)
//
// Endpoints:
//   GET /health            -> 200 (unauthenticated, like real Headscale)
//   GET /version           -> {"version": "v0.27.0"}
//   GET /api/v1/apikey     -> one non-expired key (Bearer auth required)
//   GET /api/v1/node       -> {"nodes": [...3 nodes...]}
//   GET /api/v1/user       -> {"users": [...2 users...]}
//   GET /api/v1/policy     -> {"policy": "<acl>", "updatedAt": ...}
//   GET /api/v1/preauthkey -> {"preAuthKeys": []}

import { createServer } from "node:http";

const PORT = Number.parseInt(process.env.STUB_PORT ?? "5001", 10);
const API_KEY = "bench-api-key";

const future = new Date(Date.now() + 365 * 24 * 3600 * 1000).toISOString();

const nodes = [
  {
    id: "1",
    hostname: "bench-node-1",
    name: "bench-node-1",
    user: { id: "1", name: "bench-user" },
    ipAddresses: ["100.64.0.1"],
    givenName: "bench-node-1",
    lastSeen: new Date().toISOString(),
    nodeKey: "nodekey:",
    machineKey: "mkey:",
    discoKey: "discokey:",
    tags: [],
    expiry: null,
    createdAt: "2026-01-01T00:00:00Z",
    registerMethod: "REGISTER_METHOD_AUTH_KEY",
    approvedRoutes: [],
    availableRoutes: [],
    subnetRoutes: [],
    online: true,
    forcedTags: [],
    validTags: [],
  },
  {
    id: "2",
    hostname: "bench-node-2",
    name: "bench-node-2",
    user: { id: "1", name: "bench-user" },
    ipAddresses: ["100.64.0.2"],
    givenName: "bench-node-2",
    lastSeen: new Date().toISOString(),
    nodeKey: "nodekey:",
    machineKey: "mkey:",
    discoKey: "discokey:",
    tags: [],
    expiry: null,
    createdAt: "2026-01-01T00:00:00Z",
    registerMethod: "REGISTER_METHOD_AUTH_KEY",
    approvedRoutes: [],
    availableRoutes: [],
    subnetRoutes: [],
    online: true,
    forcedTags: [],
    validTags: [],
  },
  {
    id: "3",
    hostname: "bench-node-3",
    name: "bench-node-3",
    user: { id: "2", name: "other-user" },
    ipAddresses: ["100.64.0.3"],
    givenName: "bench-node-3",
    lastSeen: new Date().toISOString(),
    nodeKey: "nodekey:",
    machineKey: "mkey:",
    discoKey: "discokey:",
    tags: [],
    expiry: null,
    createdAt: "2026-01-01T00:00:00Z",
    registerMethod: "REGISTER_METHOD_AUTH_KEY",
    approvedRoutes: [],
    availableRoutes: [],
    subnetRoutes: [],
    online: true,
    forcedTags: [],
    validTags: [],
  },
];

const users = [
  { id: "1", name: "bench-user", email: "", displayName: "Bench User" },
  { id: "2", name: "other-user", email: "", displayName: "Other User" },
];

function json(res, status, obj) {
  const body = JSON.stringify(obj);
  res.writeHead(status, {
    "Content-Type": "application/json",
    "Content-Length": Buffer.byteLength(body),
  });
  res.end(body);
}

function authorized(req) {
  return req.headers.authorization === `Bearer ${API_KEY}`;
}

const server = createServer((req, res) => {
  const url = new URL(req.url, "http://localhost");
  const { pathname } = url;

  if (pathname === "/health") return json(res, 200, {});
  if (pathname === "/version") return json(res, 200, { version: "v0.27.0" });

  if (!authorized(req)) return json(res, 401, { message: "unauthorized" });

  if (pathname === "/api/v1/apikey")
    return json(res, 200, {
      apiKeys: [{ id: "1", prefix: "bench", expiration: future }],
    });
  if (pathname === "/api/v1/node") return json(res, 200, { nodes });
  if (pathname === "/api/v1/user") return json(res, 200, { users });
  if (pathname === "/api/v1/policy")
    return json(res, 200, {
      policy: "// bench stub policy\n",
      updatedAt: new Date().toISOString(),
    });
  if (pathname === "/api/v1/preauthkey") return json(res, 200, { preAuthKeys: [] });

  return json(res, 404, { message: "stub: not implemented" });
});

server.listen(PORT, "127.0.0.1", () => {
  console.log(`[stub-headscale] listening on 127.0.0.1:${PORT}`);
});
