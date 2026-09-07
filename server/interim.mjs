#!/usr/bin/env node
// MARK: Phase 0 interim server
//
// Serves the SPA (`ssr: false`) build while the Go server is being written:
//   1. `${PREFIX}` → 302 `${PREFIX}/` (mirrors runtime/http.ts)
//   2. Reverse-proxies server-driven flows to the existing Node SSR server:
//      login POST, logout, OIDC, browser SSH/RDP pages, /api/* utilities,
//      /healthz, the live SSE stream
//   3. Serves the stub `/api/v1/*` JSON API (server/interim-stub.mjs) that the
//      SPA's clientLoaders fetch — same contract the Go server will implement
//   4. Serves `build/client` statically with the production cache semantics
//      (immutable `/assets/*`, 1h everything else; traversal → 404)
//   5. SPA fallback: every other GET under the prefix serves `index.html`
//      (Cache-Control: no-cache)
//
// This server is deleted once the Go server takes over (Phase 6).
//
//   INTERIM_PORT        listen port (default 3000)
//   INTERIM_SSR_HOST    SSR proxy upstream host (default 127.0.0.1)
//   INTERIM_SSR_PORT    SSR proxy upstream port (default 3001)
//   INTERIM_PREFIX      URL prefix, must match vite `PREFIX` (default /admin)
//   INTERIM_CLIENT_DIR  SPA static dir (default build/client)
import { createReadStream } from "node:fs";
import { stat } from "node:fs/promises";
import { createServer, request as httpRequest } from "node:http";
import { extname, normalize, resolve, sep } from "node:path";

import { handleStubRequest } from "./interim-stub.mjs";

const PORT = parseInt(process.env.INTERIM_PORT ?? "3000", 10);
const SSR_HOST = process.env.INTERIM_SSR_HOST ?? "127.0.0.1";
const SSR_PORT = parseInt(process.env.INTERIM_SSR_PORT ?? "3001", 10);
const PREFIX = process.env.INTERIM_PREFIX ?? "/admin";
const CLIENT_DIR = resolve(process.env.INTERIM_CLIENT_DIR ?? "build/client");
const V1_PREFIX = `${PREFIX}/api/v1`;

const MIME = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".mjs": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".json": "application/json",
  ".map": "application/json",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".ico": "image/x-icon",
  ".webp": "image/webp",
  ".woff2": "font/woff2",
  ".wasm": "application/wasm",
  ".txt": "text/plain; charset=utf-8",
};

function log(...args) {
  console.log("[interim]", ...args);
}

// Server-driven flows that stay on the existing Node server in the interim.
// Everything else under the prefix is the SPA (static shell or stub API).
function isProxied(pathname, method) {
  // POST /login is the server-driven login action. The SPA's clientAction
  // issues it as a React Router single-fetch data request (`/login.data`)
  // so the action result comes back as JSON instead of a re-rendered
  // document (a document POST would have it swallowed into the page).
  if (
    (pathname === `${PREFIX}/login` || pathname === `${PREFIX}/login.data`) &&
    method === "POST"
  ) {
    return true;
  }
  if (pathname === `${PREFIX}/logout`) return true;
  if (pathname === `${PREFIX}/oidc/start` || pathname === `${PREFIX}/oidc/callback`) return true;
  if (
    pathname === `${PREFIX}/ssh` ||
    pathname === `${PREFIX}/rdp` ||
    pathname.startsWith(`${PREFIX}/ssh/`) ||
    pathname.startsWith(`${PREFIX}/rdp/`)
  ) {
    return true;
  }
  return (
    pathname === `${PREFIX}/api/info` ||
    pathname === `${PREFIX}/healthz` ||
    pathname === `${PREFIX}/events/live` ||
    pathname === `${PREFIX}/api/rdp-gateway` ||
    pathname === `${PREFIX}/api/color-scheme`
  );
}

function proxyToSSR(req, res) {
  const headers = { ...req.headers };
  headers.host = `${SSR_HOST}:${SSR_PORT}`;
  // The rdp-gateway action derives the caller IP from X-Forwarded-For.
  if (!headers["x-forwarded-for"] && req.socket.remoteAddress) {
    headers["x-forwarded-for"] = req.socket.remoteAddress;
  }
  delete headers["connection"];
  const proxyReq = httpRequest(
    {
      host: SSR_HOST,
      port: SSR_PORT,
      method: req.method,
      path: req.url,
      headers,
    },
    (proxyRes) => {
      res.writeHead(proxyRes.statusCode ?? 502, proxyRes.headers);
      proxyRes.pipe(res);
    },
  );
  proxyReq.on("error", (err) => {
    log("proxy to SSR failed: %s", err.message);
    if (!res.headersSent) {
      res.writeHead(502, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "Upstream SSR server unavailable" }));
    } else {
      res.destroy(err);
    }
  });
  req.pipe(proxyReq);
}

// Static serving with the production semantics from runtime/http.ts.
// Returns true when the request was served.
async function serveStatic(req, res, pathname) {
  const prefix = `${PREFIX}/`;
  const rel = pathname.slice(prefix.length);
  if (!rel || rel.endsWith("/")) return false;

  // Traversal attempts 404 — never SPA fallback (matches the Go contract).
  const normalized = normalize(rel);
  if (normalized === ".." || normalized.startsWith(`..${sep}`) || normalized.startsWith("../")) {
    res.writeHead(404, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: "Not found" }));
    return true;
  }

  const file = resolve(CLIENT_DIR, normalized);
  if (file !== CLIENT_DIR && !file.startsWith(CLIENT_DIR + sep)) {
    res.writeHead(404, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: "Not found" }));
    return true;
  }

  let st;
  try {
    st = await stat(file);
  } catch {
    return false;
  }
  if (!st.isFile()) return false;

  const isAsset = pathname.startsWith(`${prefix}assets/`);
  res.setHeader(
    "Cache-Control",
    isAsset ? "public, max-age=31536000, immutable" : "public, max-age=3600",
  );
  res.setHeader("Content-Type", MIME[extname(file)] ?? "application/octet-stream");
  res.setHeader("Content-Length", String(st.size));
  res.setHeader("Last-Modified", st.mtime.toUTCString());
  res.statusCode = 200;

  if (req.method === "HEAD") {
    res.end();
    return true;
  }
  await new Promise((resolvePromise, reject) => {
    const stream = createReadStream(file);
    stream.on("error", reject);
    stream.on("end", () => resolvePromise());
    stream.pipe(res);
  });
  return true;
}

async function serveIndex(res) {
  const file = resolve(CLIENT_DIR, "index.html");
  let st;
  try {
    st = await stat(file);
  } catch {
    res.writeHead(500, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: "SPA build not found" }));
    return;
  }
  // SPA shell must never be cached — it references hashed asset filenames.
  res.setHeader("Cache-Control", "no-cache");
  res.setHeader("Content-Type", "text/html; charset=utf-8");
  res.setHeader("Content-Length", String(st.size));
  res.statusCode = 200;
  await new Promise((resolvePromise, reject) => {
    const stream = createReadStream(file);
    stream.on("error", reject);
    stream.on("end", () => resolvePromise());
    stream.pipe(res);
  });
}

const server = createServer(async (req, res) => {
  let url;
  try {
    url = new URL(req.url ?? "/", "http://localhost");
  } catch {
    res.writeHead(400);
    res.end();
    return;
  }
  const pathname = url.pathname;
  const method = req.method ?? "GET";

  // 1. Basename redirect, verbatim from runtime/http.ts.
  if (pathname === PREFIX) {
    res.writeHead(302, { Location: `${PREFIX}/${url.search}` });
    res.end();
    return;
  }

  // 2. Server-driven flows → existing Node server.
  if (isProxied(pathname, method)) {
    proxyToSSR(req, res);
    return;
  }

  // 3. Stub v1 JSON API for the SPA's clientLoaders.
  if (pathname === V1_PREFIX || pathname.startsWith(`${V1_PREFIX}/`)) {
    const subpath = pathname.slice(V1_PREFIX.length) || "/";
    try {
      await handleStubRequest(req, res, subpath);
    } catch (err) {
      log("stub API failed: %o", err);
      if (!res.headersSent) {
        res.writeHead(500, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "Internal stub error" }));
      }
    }
    return;
  }

  // 4/5. Static assets, then SPA fallback for GETs under the prefix.
  if ((method === "GET" || method === "HEAD") && pathname.startsWith(`${PREFIX}/`)) {
    try {
      if (await serveStatic(req, res, pathname)) return;
    } catch (err) {
      log("static serve failed: %o", err);
      if (!res.headersSent) {
        res.writeHead(500);
        res.end("Internal Server Error");
      }
      return;
    }
    if (method === "GET") {
      await serveIndex(res);
      return;
    }
  }

  res.writeHead(404, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ error: "Not found" }));
});

server.listen(PORT, "127.0.0.1", () => {
  log(`listening on http://127.0.0.1:${PORT}${PREFIX}/ (proxy → ${SSR_HOST}:${SSR_PORT})`);
});

process.once("SIGINT", () => process.exit(0));
process.once("SIGTERM", () => process.exit(0));
