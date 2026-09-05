// MARK: Production-server smoke contract
//
// Boots the exact artifact CI ships (`build/server/index.js`) and proves
// the production request pipeline works end to end: the React Router
// loader pipeline answers, the basename redirect fires, and the custom
// static-asset handler serves files with the expected cache behavior.
//
// Requires a prior `pnpm run build` (CI builds before testing); the whole
// file is skipped when the build output is absent so `test:unit`-style
// runs without a build are unaffected.
//
// Headscale is intentionally unreachable here: /healthz honestly reports
// ERROR in that case. The contract asserts the server *responds* with a
// well-formed payload, not that the deployment is healthy.

import { existsSync, readdirSync } from "node:fs";
import { request as httpRequest } from "node:http";
import { join } from "node:path";

import { afterAll, beforeAll, describe, expect, test } from "vitest";

import {
  CLIENT_DIR,
  PREFIX,
  SERVER_ENTRY,
  startTestServer,
  type TestServer,
} from "./setup/server";

describe.skipIf(!existsSync(SERVER_ENTRY))("production server smoke contract", () => {
  let server: TestServer;

  beforeAll(async () => {
    server = await startTestServer();
  }, 90_000);

  afterAll(async () => {
    await server?.stop();
  });

  /** GET a raw (non-normalized) path straight through a socket. */
  function rawGet(path: string): Promise<{ status: number; body: string }> {
    return new Promise((resolvePromise, reject) => {
      const req = httpRequest(
        { host: "127.0.0.1", port: server.port, path, method: "GET" },
        (res) => {
          let body = "";
          res.on("data", (chunk: Buffer) => {
            body += chunk.toString();
          });
          res.on("end", () => resolvePromise({ status: res.statusCode ?? 0, body }));
        },
      );
      req.on("error", reject);
      req.end();
    });
  }

  test("answers /healthz with a JSON status payload", async () => {
    const res = await fetch(`${server.baseUrl}/healthz`);
    expect(res.headers.get("content-type")).toContain("application/json");
    const body = (await res.json()) as { status?: string };
    expect(["OK", "ERROR"]).toContain(body.status);
  });

  test("redirects the bare basename to the trailing-slash form", async () => {
    const res = await fetch(server.baseUrl, { redirect: "manual" });
    expect(res.status).toBe(302);
    expect(res.headers.get("location")).toBe(`${PREFIX}/`);
  });

  test("serves immutable hashed assets with long-lived cache headers", async () => {
    const assetsDir = join(CLIENT_DIR, "assets");
    const assets = existsSync(assetsDir)
      ? readdirSync(assetsDir).filter((f) => f.endsWith(".js"))
      : [];
    expect(assets.length).toBeGreaterThan(0);

    const res = await fetch(`${server.baseUrl}/assets/${assets[0]}`);
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("javascript");
    expect(res.headers.get("cache-control")).toBe("public, max-age=31536000, immutable");
    expect((await res.text()).length).toBeGreaterThan(0);
  });

  test("answers HEAD requests with headers and no body", async () => {
    const assetsDir = join(CLIENT_DIR, "assets");
    const assets = existsSync(assetsDir)
      ? readdirSync(assetsDir).filter((f) => f.endsWith(".js"))
      : [];
    expect(assets.length).toBeGreaterThan(0);

    const res = await fetch(`${server.baseUrl}/assets/${assets[0]}`, { method: "HEAD" });
    expect(res.status).toBe(200);
    expect(res.headers.get("content-length")).toBeTruthy();
    expect(await res.text()).toBe("");
  });

  test("serves non-asset public files with short-lived cache headers", async () => {
    const favicon = join(CLIENT_DIR, "favicon.ico");
    if (!existsSync(favicon)) {
      return;
    }
    const res = await fetch(`${server.baseUrl}/favicon.ico`);
    expect(res.status).toBe(200);
    expect(res.headers.get("cache-control")).toBe("public, max-age=3600");
  });

  test("rejects path traversal outside the static root", async () => {
    // Send the path through a raw socket: fetch/undici would normalize
    // dot segments client-side, which would not exercise the server's
    // own traversal guard in runtime/http.ts.
    const { status, body } = await rawGet("/admin/%2e%2e/%2e%2e/package.json");
    expect(status).toBe(404);
    expect(body).not.toContain("\"name\": \"headplane\"");
  });
});
