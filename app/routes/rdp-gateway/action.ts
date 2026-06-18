import { data } from "react-router";

import { Capabilities } from "~/server/web/roles";
import log from "~/utils/log";

import type { Route } from "./+types/action";

// Derive the caller's public IP from the reverse-proxy headers Caddy sets.
// Falls back to "0.0.0.0" when neither header is present (e.g. direct
// connections in development) — the gateway webhook should handle that
// gracefully (e.g. skip source-IP allowlisting).
//
// Security note: X-Forwarded-For is trusted as-is. This is safe when
// Headplane sits behind a trusted reverse proxy (e.g. Caddy, Nginx) that
// strips and rewrites XFF before forwarding. If Headplane is exposed
// directly to the internet, a caller could spoof this header to open the
// relay scoped to an arbitrary IP. Deploy behind a reverse proxy.
function getCallerIp(request: Request): string {
  const xff = request.headers.get("x-forwarded-for");
  if (xff) {
    return xff.split(",")[0].trim();
  }
  const realIp = request.headers.get("x-real-ip");
  if (realIp) {
    return realIp.trim();
  }
  return "0.0.0.0";
}

// This is an action-only endpoint. Reject GET requests gracefully rather
// than letting React Router produce an unhelpful 405 error boundary.
export function loader() {
  return data(null, { status: 405, headers: { Allow: "POST" } });
}

export async function action({ request, context }: Route.ActionArgs) {
  const principal = await context.auth.require(request);

  // Gate to users who can write machine state. This includes owner, admin,
  // it_admin, and network_admin roles. write_machines is intentionally used
  // over write_network so that IT admins (who manage Windows machines) can
  // use this feature; write_network is for DNS/subnet/exit-node config which
  // is a separate concern.
  if (!context.auth.can(principal, Capabilities.write_machines)) {
    return data({ success: false, error: "Insufficient permissions" }, 403);
  }

  if (context.rdpGateway.state !== "enabled") {
    return data({ success: false, error: "RDP gateway is not configured on this server" }, 404);
  }

  const formData = await request.formData();
  const actionId = formData.get("action_id")?.toString();
  const targetIp = formData.get("target_ip")?.toString();
  const hostname = formData.get("hostname")?.toString();

  if (!actionId || !targetIp || !hostname) {
    return data({ success: false, error: "Missing required fields" }, 400);
  }

  const gateway = context.rdpGateway.value;

  switch (actionId) {
    case "enable": {
      const callerIp = getCallerIp(request);
      const rawMins = formData.get("timeout_mins")?.toString();
      const timeoutMins = rawMins
        ? Math.min(Math.max(parseInt(rawMins, 10) || 180, 30), 480)
        : undefined;
      try {
        const result = await gateway.enable(targetIp, hostname, callerIp, timeoutMins);
        return { success: true, ...result };
      } catch (err) {
        // Log detail server-side; return a generic message to the client so
        // internal relay infrastructure details don't leak to the browser.
        log.error("api", "enable failed for %s: %s", hostname, String(err));
        return data({ success: false, error: "Gateway request failed. Check server logs." }, 502);
      }
    }

    case "disable": {
      try {
        await gateway.disable(targetIp, hostname);
        return { success: true };
      } catch (err) {
        log.error("api", "disable failed for %s: %s", hostname, String(err));
        return data({ success: false, error: "Gateway request failed. Check server logs." }, 502);
      }
    }

    default:
      return data({ success: false, error: "Unknown action" }, 400);
  }
}
