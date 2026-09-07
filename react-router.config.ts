import type { Config } from "@react-router/dev/config";

export default {
  basename: "/admin/",
  // Phase 0: the SPA build (HEADPLANE_SPA_BUILD=1) ships `ssr: false` — no
  // server bundle, static `build/client` only. The default build stays
  // ssr:true for the interim SSR proxy that keeps serving the
  // server-driven flows (login POST, OIDC, SSH/RDP minting, /api/*).
  ssr: process.env.HEADPLANE_SPA_BUILD !== "1",
  future: {
    unstable_optimizeDeps: true,
    v8_splitRouteModules: "enforce",
  },
} satisfies Config;
