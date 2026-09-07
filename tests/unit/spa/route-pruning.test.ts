// Phase 0 (SPA conversion): the SPA build ships a pruned route tree —
// server-driven flows (healthz, /api/*, /events/live, logout, OIDC,
// ssh/rdp) stay on the Node server and are reached via the interim reverse
// proxy, so they must not be SPA routes. The full build keeps them.
import { describe, expect, test, vi } from "vitest";

interface FlatRoute {
  path: string;
  file: string;
}

function flatten(routes: any[], parentPath = ""): FlatRoute[] {
  const out: FlatRoute[] = [];
  for (const r of routes) {
    const path = `${parentPath}${r.path ?? ""}`;
    out.push({ path, file: r.file ?? "" });
    if (r.children) out.push(...flatten(r.children, path.endsWith("/") ? path : `${path}/`));
  }
  return out;
}

async function loadRoutes(spaBuild: boolean): Promise<FlatRoute[]> {
  vi.resetModules();
  process.env.HEADPLANE_SPA_BUILD = spaBuild ? "1" : "";
  const mod = await import("~/routes");
  return flatten(mod.default);
}

// Route modules that must never ship in the SPA browser bundle.
const SERVER_ONLY_FILES = [
  "routes/util/healthz.ts",
  "routes/util/info.ts",
  "routes/util/color-scheme.ts",
  "routes/rdp-gateway/action.ts",
  "routes/util/live.ts",
  "routes/auth/logout.ts",
  "routes/auth/oidc-callback.ts",
  "routes/auth/oidc-start.ts",
  "routes/ssh/page.tsx",
  "routes/rdp/page.tsx",
];

describe("SPA route pruning", () => {
  test("SPA build excludes server-only route modules", async () => {
    const routes = await loadRoutes(true);
    const files = routes.map((r) => r.file);
    for (const f of SERVER_ONLY_FILES) {
      expect(files).not.toContain(f);
    }
  });

  test("SPA build keeps the login page and app routes", async () => {
    const routes = await loadRoutes(true);
    const files = routes.map((r) => r.file);
    expect(files).toContain("routes/auth/login/page.tsx");
    expect(files).toContain("routes/home.tsx");
    expect(files).toContain("routes/machines/overview.tsx");
  });

  test("non-SPA build keeps the server-only route modules", async () => {
    const routes = await loadRoutes(false);
    const files = routes.map((r) => r.file);
    for (const f of SERVER_ONLY_FILES) {
      expect(files).toContain(f);
    }
  });
});
