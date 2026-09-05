// MARK: External-boundary contract
//
// Pins the behavior of the process's external edges so a runtime swap
// cannot silently change them: the undici-based Headscale transport
// (success, API errors, connection failures, disposal), Docker daemon
// discovery over a unix socket, and child-process supervision with the
// same stdio/env/exit semantics hp-agent relies on.
//
// All peers are deterministic in-test fakes; no Docker daemon or
// Headscale is required.

import { spawn, type ChildProcess } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";

import { afterAll, beforeAll, describe, expect, test } from "vitest";

import DockerIntegration from "~/server/config/integration/docker";
import { createTransport } from "~/server/headscale/api/transport";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Extract the HTTP status from a react-router `data()` thrown value. */
function thrownStatus(err: unknown): number | undefined {
  const e = err as { init?: { status?: number }; status?: number } | null;
  return e?.init?.status ?? e?.status;
}

/** Extract the payload from a react-router `data()` thrown value. */
function thrownData<T>(err: unknown): T {
  return (err as { data?: T } | null)?.data as T;
}

function waitForChildExit(
  child: ChildProcess,
  timeoutMs = 10_000,
): Promise<{ code: number | null; signal: NodeJS.Signals | null }> {
  return new Promise((resolvePromise, reject) => {
    const timer = setTimeout(() => {
      reject(new Error("timed out waiting for child exit"));
    }, timeoutMs);
    timer.unref?.();
    child.once("exit", (code, signal) => {
      clearTimeout(timer);
      resolvePromise({ code, signal });
    });
    child.once("error", (err) => {
      clearTimeout(timer);
      reject(err);
    });
  });
}

async function readLines(child: ChildProcess, count: number): Promise<string[]> {
  const lines: string[] = [];
  const rl = createInterface({ input: child.stdout! });
  for await (const line of rl) {
    lines.push(line);
    if (lines.length >= count) break;
  }
  rl.close();
  return lines;
}

// ---------------------------------------------------------------------------
// Undici Headscale transport against a fake Headscale
// ---------------------------------------------------------------------------

describe("undici transport contract", () => {
  let server: Server;
  let port: number;
  let baseUrl: string;

  beforeAll(async () => {
    server = createServer((req, res) => {
      if (req.url === "/version") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ version: "0.28.0" }));
      } else if (req.url === "/health") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end("{}");
      } else if (req.url === "/api/v1/nodes") {
        res.writeHead(400, { "content-type": "application/json" });
        res.end(JSON.stringify({ message: "bad request" }));
      } else {
        res.writeHead(404);
        res.end();
      }
    });
    await new Promise<void>((resolvePromise) => server.listen(0, "127.0.0.1", resolvePromise));
    const addr = server.address();
    port = typeof addr === "object" && addr ? addr.port : 0;
    baseUrl = `http://127.0.0.1:${port}`;
  });

  afterAll(async () => {
    await new Promise<void>((resolvePromise) => server.close(() => resolvePromise()));
  });

  test("health() is true when /health answers 200", async () => {
    const transport = await createTransport({ url: baseUrl });
    try {
      await expect(transport.health()).resolves.toBe(true);
    } finally {
      await transport.dispose();
    }
  });

  test("getPublic() returns parsed JSON for public endpoints", async () => {
    const transport = await createTransport({ url: baseUrl });
    try {
      await expect(transport.getPublic<{ version: string }>("/version")).resolves.toEqual({
        version: "0.28.0",
      });
    } finally {
      await transport.dispose();
    }
  });

  test("API errors surface as a 502 data() response with the upstream status", async () => {
    const transport = await createTransport({ url: baseUrl });
    try {
      const err = await transport
        .request({ method: "GET", path: "v1/nodes", apiKey: "test-key" })
        .then(
          () => {
            throw new Error("expected request() to throw");
          },
          (e: unknown) => e,
        );
      expect(thrownStatus(err)).toBe(502);
      const payload = thrownData<{ statusCode: number; requestUrl: string }>(err);
      expect(payload.statusCode).toBe(400);
      expect(payload.requestUrl).toContain("v1/nodes");
    } finally {
      await transport.dispose();
    }
  });

  test("connection failures never throw from health() and map to error payloads", async () => {
    const transport = await createTransport({ url: "http://127.0.0.1:1" });
    try {
      await expect(transport.health()).resolves.toBe(false);

      const err = await transport.getPublic("/version").then(
        () => {
          throw new Error("expected getPublic() to throw");
        },
        (e: unknown) => e,
      );
      expect(thrownStatus(err)).toBe(502);
      const payload = thrownData<{ requestUrl: string; errorCode: string }>(err);
      expect(payload.requestUrl).toContain("GET");
      expect(payload.errorCode).toBeTruthy();
    } finally {
      await transport.dispose();
    }
  });

  test("dispose() closes the agent without throwing", async () => {
    const transport = await createTransport({ url: baseUrl });
    await expect(transport.dispose()).resolves.toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// Docker daemon discovery over a fake unix socket
// ---------------------------------------------------------------------------

describe("Docker integration discovery contract", () => {
  let dir: string;
  let sockPath: string;
  let server: Server;

  beforeAll(async () => {
    dir = await mkdtemp(join(tmpdir(), "headplane-docker-test-"));
    sockPath = join(dir, "docker.sock");
    server = createServer((req, res) => {
      const url = req.url ?? "";
      if (url === "/version") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ ApiVersion: "1.44" }));
      } else if (url.startsWith("/v1.44/containers/json")) {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify([{ Id: "abc123", Names: ["/headscale"] }]));
      } else {
        res.writeHead(404);
        res.end();
      }
    });
    await new Promise<void>((resolvePromise) => server.listen(sockPath, resolvePromise));
  });

  afterAll(async () => {
    await new Promise<void>((resolvePromise) => server.close(() => resolvePromise()));
    await rm(dir, { recursive: true, force: true });
  });

  function makeIntegration(socket: string): DockerIntegration {
    return new DockerIntegration({
      enabled: true,
      container_label: "me.tale.headplane.target=headscale",
      socket,
    });
  }

  test("isAvailable() validates the API version over the unix socket", async () => {
    const integration = makeIntegration(`unix://${sockPath}`);
    await expect(integration.isAvailable()).resolves.toBe(true);
  });

  test("getContainerName() resolves the labeled container id", async () => {
    const integration = makeIntegration(`unix://${sockPath}`);
    expect(await integration.isAvailable()).toBe(true);
    await expect(
      integration.getContainerName("me.tale.headplane.target", "headscale"),
    ).resolves.toBe("abc123");
  });

  test("isAvailable() is false when the socket does not exist", async () => {
    const integration = makeIntegration("unix:///nonexistent/docker.sock");
    await expect(integration.isAvailable()).resolves.toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Child-process supervision with the hp-agent spawn semantics
// ---------------------------------------------------------------------------

describe("child-process contract", () => {
  let dir: string;
  let fakeAgent: string;

  beforeAll(async () => {
    dir = await mkdtemp(join(tmpdir(), "headplane-proc-test-"));
    fakeAgent = join(dir, "fake-agent.mjs");
    // Mimics the hp-agent child protocol: line-based stdout, env-driven
    // behavior, numeric exit codes, stderr diagnostics.
    await writeFile(
      fakeAgent,
      [
        "const mode = process.env.FAKE_MODE ?? 'ok';",
        "console.log('ready');",
        "console.log(`FAKE_MODE=${process.env.FAKE_MODE ?? ''}`);",
        "if (mode === 'fail') { console.error('boom'); process.exit(3); }",
        "if (mode === 'hang') { setInterval(() => {}, 1000); }",
        "else { setTimeout(() => process.exit(0), 50); }",
        "",
      ].join("\n"),
    );
  });

  afterAll(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  function spawnFake(mode: string): ChildProcess {
    return spawn(process.execPath, [fakeAgent], {
      env: { ...process.env, FAKE_MODE: mode },
      stdio: ["pipe", "pipe", "pipe"],
    });
  }

  test("stdout lines and environment follow the agent protocol", async () => {
    const child = spawnFake("ok");
    try {
      const lines = await readLines(child, 2);
      expect(lines).toEqual(["ready", "FAKE_MODE=ok"]);
      const { code } = await waitForChildExit(child);
      expect(code).toBe(0);
    } finally {
      child.kill("SIGKILL");
    }
  });

  test("non-zero exit codes and stderr are observable", async () => {
    const child = spawnFake("fail");
    const stderr: string[] = [];
    child.stderr?.on("data", (chunk: Buffer) => stderr.push(chunk.toString()));
    try {
      const { code } = await waitForChildExit(child);
      expect(code).toBe(3);
      expect(stderr.join("")).toContain("boom");
    } finally {
      child.kill("SIGKILL");
    }
  });

  test("SIGTERM terminates a running child and reports the signal", async () => {
    const child = spawnFake("hang");
    try {
      const lines = await readLines(child, 1);
      expect(lines).toEqual(["ready"]);
      child.kill("SIGTERM");
      const { code, signal } = await waitForChildExit(child);
      expect(code).toBeNull();
      expect(signal).toBe("SIGTERM");
    } finally {
      child.kill("SIGKILL");
    }
  });

  test("spawning a missing executable surfaces ENOENT", async () => {
    const child = spawn("/nonexistent/headplane-binary-xyz", [], { stdio: "ignore" });
    const err = await waitForChildExit(child).then(
      () => {
        throw new Error("expected spawn to fail");
      },
      (e: unknown) => e,
    );
    expect((err as NodeJS.ErrnoException).code).toBe("ENOENT");
  });
});
