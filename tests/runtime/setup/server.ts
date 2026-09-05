// MARK: Runtime contract test harness
//
// Spawns the production build (`build/server/index.js`) as a real child
// process so the runtime contracts exercise the same artifact CI ships.
// The JS runtime executing the server is selected with HP_TEST_RUNTIME
// ("node" by default, "bun" in the Bun experiment lane), which lets the
// identical suite run against both runtimes later.
//
// Each server gets an isolated temp data dir and a minimal env-only
// configuration: no config file, no reachable Headscale (the health
// endpoint honestly reports ERROR in that case while still proving the
// server pipeline is up), no integrations, no agent.

import { spawn, type ChildProcess } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { get as httpsGet } from "node:https";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { createServer } from "node:net";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";

export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
export const SERVER_ENTRY = join(REPO_ROOT, "build", "server", "index.js");
export const CLIENT_DIR = join(REPO_ROOT, "build", "client");

// Basename prefix baked into the build (vite.config.ts default).
export const PREFIX = process.env.HP_TEST_PREFIX ?? "/admin";

// JS runtime used to execute the built server.
export const RUNTIME_BIN = process.env.HP_TEST_RUNTIME ?? "node";

// Exactly 32 characters, per the server.cookie_secret schema.
const COOKIE_SECRET = "0123456789abcdef0123456789abcdef";

export interface TestServer {
  proc: ChildProcess;
  port: number;
  /** e.g. http://127.0.0.1:4123/admin */
  baseUrl: string;
  /** Isolated temp dir used for data_path (and scratch files). */
  dir: string;
  stdout: string[];
  stderr: string[];
  stop(signal?: NodeJS.Signals): Promise<{ code: number | null; signal: NodeJS.Signals | null }>;
}

export interface StartOptions {
  extraEnv?: Record<string, string>;
  /** The server boots with TLS termination; poll its health over HTTPS. */
  tls?: boolean;
}

/** Minimal env-only configuration shared by all runtime contract servers. */
export function testEnv(
  dir: string,
  port: number,
  extraEnv: Record<string, string> = {},
): NodeJS.ProcessEnv {
  return {
    ...process.env,
    HEADPLANE_SERVER__HOST: "127.0.0.1",
    HEADPLANE_SERVER__PORT: String(port),
    HEADPLANE_SERVER__COOKIE_SECRET: COOKIE_SECRET,
    HEADPLANE_SERVER__DATA_PATH: dir,
    HEADPLANE_HEADSCALE__URL: "http://127.0.0.1:1",
    HEADPLANE_CONFIG_PATH: join(dir, "nonexistent-config.yaml"),
    ...extraEnv,
  };
}

export async function getFreePort(): Promise<number> {
  return new Promise((resolvePromise, reject) => {
    const probe = createServer();
    probe.on("error", reject);
    probe.listen(0, "127.0.0.1", () => {
      const addr = probe.address();
      probe.close(() => {
        if (addr != null && typeof addr === "object") {
          resolvePromise(addr.port);
        } else {
          reject(new Error("could not determine free port"));
        }
      });
    });
  });
}

export function waitForExit(
  proc: ChildProcess,
  timeoutMs: number,
): Promise<{ code: number | null; signal: NodeJS.Signals | null }> {
  return new Promise((resolvePromise, reject) => {
    if (proc.exitCode !== null || proc.signalCode !== null) {
      resolvePromise({ code: proc.exitCode, signal: proc.signalCode });
      return;
    }
    const timer = setTimeout(() => {
      proc.off("exit", onExit);
      reject(new Error(`timed out after ${timeoutMs}ms waiting for process exit`));
    }, timeoutMs);
    // The timer alone must not hold the test runner open.
    // NOTE: this must be the global callback-style setTimeout (a real
    // timer handle), not the promisified node:timers/promises one.
    timer.unref();
    const onExit = (code: number | null, signal: NodeJS.Signals | null) => {
      clearTimeout(timer);
      resolvePromise({ code, signal });
    };
    proc.once("exit", onExit);
  });
}

async function waitForHttp(url: string, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let lastError: unknown;
  // TLS-enabled servers present self-signed test certificates; bypass
  // verification the same way the TLS contract's own client does.
  const insecureTls = url.startsWith("https://");
  while (Date.now() < deadline) {
    try {
      if (insecureTls) {
        await new Promise<void>((resolvePromise, reject) => {
          const req = httpsGet(url, { rejectUnauthorized: false }, (res) => {
            res.resume();
            res.once("end", () => resolvePromise());
          });
          req.once("error", reject);
          req.end();
        });
      } else {
        // Any HTTP response — including 500 from /healthz when Headscale is
        // unreachable — proves the server pipeline is accepting traffic.
        await fetch(url, { redirect: "manual" });
      }
      return;
    } catch (error) {
      lastError = error;
      await sleep(250);
    }
  }
  throw new Error(
    `server did not answer ${url} within ${timeoutMs}ms: ${String(lastError)}`,
  );
}

export async function startTestServer(options: StartOptions = {}): Promise<TestServer> {
  const dir = await mkdtemp(join(tmpdir(), "headplane-runtime-test-"));
  const port = await getFreePort();
  // A TLS-enabled server terminates HTTPS only; poll the matching scheme.
  const scheme = options.tls ? "https" : "http";
  const baseUrl = `${scheme}://127.0.0.1:${port}${PREFIX}`;

  const env = testEnv(dir, port, options.extraEnv);

  const proc = spawn(RUNTIME_BIN, [SERVER_ENTRY], {
    env,
    stdio: ["ignore", "pipe", "pipe"],
  });
  const stdout: string[] = [];
  const stderr: string[] = [];
  proc.stdout?.on("data", (chunk: Buffer) => stdout.push(chunk.toString()));
  proc.stderr?.on("data", (chunk: Buffer) => stderr.push(chunk.toString()));

  const earlyExit = new Promise<never>((_, reject) => {
    proc.once("exit", (code, signal) => {
      reject(
        new Error(
          `server exited during startup (code=${code} signal=${signal})\n` +
            `stdout:\n${stdout.join("")}\nstderr:\n${stderr.join("")}`,
        ),
      );
    });
  });
  // Swallow the rejection once startup succeeds; the stop() path observes
  // process exit through waitForExit instead.
  earlyExit.catch(() => {});

  try {
    await Promise.race([waitForHttp(`${baseUrl}/healthz`, 45_000), earlyExit]);
  } catch (error) {
    proc.kill("SIGKILL");
    await rm(dir, { recursive: true, force: true });
    throw error;
  }

  let stopped = false;
  async function stop(signal: NodeJS.Signals = "SIGTERM") {
    if (stopped) {
      return { code: proc.exitCode, signal: proc.signalCode };
    }
    stopped = true;
    proc.kill(signal);
    const result = await waitForExit(proc, 20_000);
    await rm(dir, { recursive: true, force: true }).catch(() => {});
    return result;
  }

  return { proc, port, baseUrl, dir, stdout, stderr, stop };
}
