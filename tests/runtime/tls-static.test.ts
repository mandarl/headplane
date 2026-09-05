// MARK: TLS and static-file contract
//
// Boots the production server with TLS termination enabled and proves:
// HTTPS serves correctly, HEAD/static semantics hold (content types,
// cache headers, empty HEAD bodies), path traversal is rejected, and a
// half-configured TLS setup fails fast instead of serving insecurely.
//
// Certificates are generated in-test with openssl (present on CI
// runners); the suite is skipped where openssl is unavailable.

import { execFileSync, spawn } from "node:child_process";
import { existsSync, readdirSync } from "node:fs";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { get as httpsGet } from "node:https";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterAll, beforeAll, describe, expect, test } from "vitest";

import {
  CLIENT_DIR,
  PREFIX,
  RUNTIME_BIN,
  SERVER_ENTRY,
  getFreePort,
  startTestServer,
  testEnv,
  waitForExit,
  type TestServer,
} from "./setup/server";

function opensslAvailable(): boolean {
  try {
    execFileSync("openssl", ["version"], { stdio: "ignore" });
    return true;
  } catch {
    return false;
  }
}

const runnable = existsSync(SERVER_ENTRY) && opensslAvailable();

describe.skipIf(!runnable)("TLS and static-file contract", () => {
  let server: TestServer;
  let dir: string;

  beforeAll(async () => {
    dir = await mkdtemp(join(tmpdir(), "headplane-tls-test-"));
    const cert = join(dir, "cert.pem");
    const key = join(dir, "key.pem");
    execFileSync(
      "openssl",
      [
        "req",
        "-x509",
        "-newkey",
        "rsa:2048",
        "-keyout",
        key,
        "-out",
        cert,
        "-days",
        "2",
        "-nodes",
        "-subj",
        "/CN=127.0.0.1",
        "-addext",
        "subjectAltName=IP:127.0.0.1",
      ],
      { stdio: "ignore" },
    );

    server = await startTestServer({
      extraEnv: {
        HEADPLANE_SERVER__TLS_CERT_PATH: cert,
        HEADPLANE_SERVER__TLS_KEY_PATH: key,
      },
    });
  }, 90_000);

  afterAll(async () => {
    await server?.stop();
    await rm(dir, { recursive: true, force: true });
  });

  function tlsUrl(path: string): string {
    return `https://127.0.0.1:${server.port}${PREFIX}${path}`;
  }

  /** fetch() equivalent for self-signed HTTPS in tests. */
  function get(
    path: string,
    method = "GET",
  ): Promise<{ status: number; headers: Record<string, string | undefined>; body: string }> {
    return new Promise((resolvePromise, reject) => {
      const req = httpsGet(
        tlsUrl(path),
        { rejectUnauthorized: false, method },
        (res) => {
          let body = "";
          res.on("data", (chunk: Buffer) => {
            body += chunk.toString();
          });
          res.on("end", () => {
            const headers: Record<string, string | undefined> = {};
            for (const [k, v] of Object.entries(res.headers)) {
              headers[k] = Array.isArray(v) ? v.join(", ") : v;
            }
            resolvePromise({ status: res.statusCode ?? 0, headers, body });
          });
        },
      );
      req.on("error", reject);
      req.end();
    });
  }

  function firstJsAsset(): string {
    const assetsDir = join(CLIENT_DIR, "assets");
    const assets = existsSync(assetsDir)
      ? readdirSync(assetsDir).filter((f) => f.endsWith(".js"))
      : [];
    expect(assets.length).toBeGreaterThan(0);
    return assets[0];
  }

  test("serves the app over HTTPS", async () => {
    const { status, body } = await get("/healthz");
    expect([200, 500]).toContain(status);
    expect(JSON.parse(body)).toHaveProperty("status");
  });

  test("HEAD over TLS returns headers with an empty body", async () => {
    const { status, headers, body } = await get(`/assets/${firstJsAsset()}`, "HEAD");
    expect(status).toBe(200);
    expect(headers["content-length"]).toBeTruthy();
    expect(body).toBe("");
  });

  test("serves assets with correct content types and cache headers", async () => {
    const { status, headers, body } = await get(`/assets/${firstJsAsset()}`);
    expect(status).toBe(200);
    expect(headers["content-type"]).toContain("javascript");
    expect(headers["cache-control"]).toBe("public, max-age=31536000, immutable");
    expect(body.length).toBeGreaterThan(0);
  });

  test("rejects path traversal over TLS", async () => {
    const { status, body } = await get("/%2e%2e/%2e%2e/package.json");
    expect(status).toBe(404);
    expect(body).not.toContain("\"name\": \"headplane\"");
  });
});

describe.skipIf(!existsSync(SERVER_ENTRY))("TLS misconfiguration contract", () => {
  test("a cert without a key fails fast instead of serving insecurely", async () => {
    const dir = await mkdtemp(join(tmpdir(), "headplane-tls-misconfig-"));
    const cert = join(dir, "cert.pem");
    if (opensslAvailable()) {
      execFileSync(
        "openssl",
        [
          "req",
          "-x509",
          "-newkey",
          "rsa:2048",
          "-keyout",
          join(dir, "key.pem"),
          "-out",
          cert,
          "-days",
          "1",
          "-nodes",
          "-subj",
          "/CN=127.0.0.1",
        ],
        { stdio: "ignore" },
      );
    } else {
      await writeFile(cert, "fake-cert");
    }

    // Bypass startTestServer: this boot is expected to fail, and the
    // helper treats early exit as an error.
    const port = await getFreePort();
    const proc = spawn(RUNTIME_BIN, [SERVER_ENTRY], {
      env: testEnv(dir, port, { HEADPLANE_SERVER__TLS_CERT_PATH: cert }),
      stdio: "ignore",
    });
    try {
      const { code } = await waitForExit(proc, 30_000);
      expect(code).toBe(1);
    } finally {
      proc.kill("SIGKILL");
      await rm(dir, { recursive: true, force: true });
    }
  }, 60_000);
});
