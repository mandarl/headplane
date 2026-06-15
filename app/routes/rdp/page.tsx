import { WifiOff } from "lucide-react";
import { data, isRouteErrorResponse, useLocation, type ShouldRevalidateFunction } from "react-router";

import Button from "~/components/button";
import Card from "~/components/card";
import Code from "~/components/code";
import { findHeadscaleUserBySubject } from "~/server/web/headscale-identity";

import type { Route } from "./+types/page";
import { RDPConsole } from "./rdp.client";
import RDPUserPrompt from "./user-prompt";

const WASM_MODULE_URL = `${__PREFIX__}/hp_rdp.wasm`;
const WASM_HELPER_URL = `${__PREFIX__}/wasm_exec.js`;

export const shouldRevalidate: ShouldRevalidateFunction = ({ currentUrl, nextUrl }) => {
  return !currentUrl.searchParams.has("user") && nextUrl.searchParams.has("user");
};

export async function loader({ request, params, context }: Route.LoaderArgs) {
  const origin = new URL(request.url).origin;
  const assets = [WASM_HELPER_URL, WASM_MODULE_URL];
  const missing: string[] = [];

  for (const file of assets) {
    const res = await fetch(`${origin}${file}`, { method: "HEAD" });
    if (!res.ok) {
      missing.push(file);
    }
  }

  if (missing.length > 0) {
    throw data({ title: "RDP Not Available", message: "hp_rdp.wasm is not available on this server." }, 405);
  }

  if (context.agents.state !== "enabled") {
    throw data(
      { title: "Agent Required", message: "The Headplane agent must be running to use Web RDP." },
      400,
    );
  }

  const { principal, api } = await context.apiForRequest(request);

  const hostname = params.id;
  const username = new URL(request.url).searchParams.get("user") || undefined;

  const nodes = await api.nodes.list();
  const node = nodes.find((n) => n.givenName === hostname);
  if (!node) {
    throw data({ title: "Node Not Found", message: `Node "${hostname}" was not found.` }, 404);
  }

  if (!node.online) {
    return { hostname, username, offline: true, node: undefined };
  }

  if (!username) {
    return { hostname, username: undefined, offline: false, node: undefined };
  }

  const users = await api.users.list();
  const hsUser =
    principal.kind === "api_key"
      ? users[0]
      : findHeadscaleUserBySubject(users, principal.user.subject, principal.profile.email);

  if (!hsUser) {
    throw data({ title: "User Not Linked", message: "Your user account is not linked to a Headscale user." }, 404);
  }

  const preAuthKey = await api.preAuthKeys.create({
    user: hsUser.id,
    ephemeral: true,
    reusable: false,
    expiration: new Date(Date.now() + 5 * 60 * 1000),
    aclTags: null,
  });

  const controlURL = context.config.headscale.public_url ?? context.config.headscale.url;
  return {
    hostname,
    username,
    offline: false,
    node: {
      ipAddress: node.ipAddresses[0],
      controlURL,
      preAuthKey: preAuthKey.key,
      ephemeralHostname: generateHostname(username),
    },
  };
}

function generateHostname(username: string) {
  const hex = crypto.randomUUID().replace(/-/g, "").slice(0, 8);
  return `rdp-${hex}-${username}`;
}

export const links: Route.LinksFunction = () => [
  {
    rel: "preload",
    href: WASM_MODULE_URL,
    as: "fetch",
    type: "application/wasm",
    crossOrigin: "anonymous",
  },
];

export default function Page({ loaderData }: Route.ComponentProps) {
  const { hostname, username, offline, node } = loaderData;
  const location = useLocation();
  const state = location.state as { password?: string; domain?: string; colorDepth?: number } | null;
  const password = state?.password ?? "";
  const domain = state?.domain ?? "";
  const colorDepth = state?.colorDepth ?? 24;

  if (offline) {
    return (
      <div className="flex h-screen w-screen items-center justify-center bg-black">
        <Card className="w-screen" variant="flat">
          <div className="flex items-center justify-between gap-4">
            <Card.Title>Node Offline</Card.Title>
            <WifiOff className="mb-2 h-6 w-6 text-red-500" />
          </div>
          <Card.Text>
            <Code>{hostname}</Code> is not currently connected to the Tailnet.
          </Card.Text>
          <Button className="mt-8 w-full" onClick={() => window.location.reload()}>
            Retry Connection
          </Button>
        </Card>
      </div>
    );
  }

  if (!username || !node) {
    return <RDPUserPrompt hostname={hostname} />;
  }

  return (
    <RDPConsole
      hostname={hostname}
      username={username}
      password={password}
      domain={domain}
      colorDepth={colorDepth}
      node={node}
    />
  );
}

export function ErrorBoundary({ error }: Route.ErrorBoundaryProps) {
  const routeError = isRouteErrorResponse(error) ? error.data : null;
  if (!routeError) throw error;

  return (
    <div className="flex h-screen w-screen items-center justify-center">
      <Card>
        <Card.Title>{routeError.title ?? "Error"}</Card.Title>
        <Card.Text>{routeError.message ?? String(error)}</Card.Text>
      </Card>
    </div>
  );
}
