import { index, layout, prefix, route } from "@react-router/dev/routes";

// Phase 0 (SPA conversion): the SPA build (`HEADPLANE_SPA_BUILD=1`) ships a
// pruned route tree. Server-driven flows — login POST, logout, OIDC, browser
// SSH/RDP key minting, /api/* utilities, healthz, the live SSE stream — stay
// on the existing Node server in the interim and are reached via the interim
// reverse proxy, so they are not SPA routes.
//
// NOTE: this file is evaluated in plain Node by the React Router Vite plugin
// (and by `react-router typegen`), never bundled by Vite — so a Vite `define`
// constant does NOT apply here. Branch on process.env directly.
const SPA_BUILD = process.env.HEADPLANE_SPA_BUILD === "1";

// Route modules with server-only loaders/actions (node imports). Never part
// of the SPA browser bundle.
const serverOnlyRoutes = SPA_BUILD
  ? []
  : [
      // Utility Routes
      route("/healthz", "routes/util/healthz.ts"),

      // API Routes
      ...prefix("/api", [
        route("/info", "routes/util/info.ts"),
        route("/color-scheme", "routes/util/color-scheme.ts"),
        route("/rdp-gateway", "routes/rdp-gateway/action.ts"),
      ]),
      ...prefix("/events", [route("/live", "routes/util/live.ts")]),

      // Authentication Routes
      route("/logout", "routes/auth/logout.ts"),
      route("/oidc/callback", "routes/auth/oidc-callback.ts"),
      route("/oidc/start", "routes/auth/oidc-start.ts"),
    ];

export default [
  ...serverOnlyRoutes,

  // Authentication Routes
  // The login page is an SPA route (client-rendered form). POST /login stays
  // server-driven: the route module re-exports the server action via
  // login-action.ts (an `export *` indirection the SPA build's validator
  // accepts — see the comment there), so both builds register this module.
  // Its clientAction forwards the form to the server endpoint instead.
  route("/login", "routes/auth/login/page.tsx"),

  // Full-screen browser SSH/RDP terminals. Client-rendered; their
  // clientLoaders fetch /api/v1/{ssh,rdp}/:id (Go server mints the
  // 5-minute ephemeral pre-auth key). Outside the app chrome layout.
  route("/ssh/:id", "routes/ssh/page.tsx"),
  route("/rdp/:id", "routes/rdp/page.tsx"),

  // All the main logged-in routes
  layout("layout/app.tsx", [
    index("routes/home.tsx"),
    ...prefix("/machines", [
      index("routes/machines/overview.tsx"),
      route("/:id", "routes/machines/machine.tsx"),
    ]),

    route("/users", "routes/users/overview.tsx"),
    route("/acls", "routes/acls/overview.tsx"),
    route("/dns", "routes/dns/overview.tsx"),

    ...prefix("/settings", [
      index("routes/settings/overview.tsx"),
      route("/auth-keys", "routes/settings/auth-keys/overview.tsx"),
      route("/restrictions", "routes/settings/restrictions/overview.tsx"),
      route("/agent", "routes/settings/agent.tsx"),
    ]),
  ]),
];
