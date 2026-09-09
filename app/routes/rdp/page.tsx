import { WifiOff } from "lucide-react";
import {
  data,
  isRouteErrorResponse,
  redirect,
  useLocation,
  type ShouldRevalidateFunction,
} from "react-router";

import Button from "~/components/button";
import Card from "~/components/card";
import Code from "~/components/code";
import { apiFetch } from "~/lib/api";

import type { Route } from "./+types/page";
import { RDPConsole } from "./rdp.client";
import RDPUserPrompt from "./user-prompt";

const WASM_MODULE_URL = `${__PREFIX__}/hp_rdp.wasm`;

export const shouldRevalidate: ShouldRevalidateFunction = ({ currentUrl, nextUrl }) => {
  return !currentUrl.searchParams.has("user") && nextUrl.searchParams.has("user");
};

interface RDPLoaderData {
  hostname: string;
  username?: string;
  offline: boolean;
  node?: {
    ipAddress: string;
    controlURL: string;
    preAuthKey: string;
    ephemeralHostname: string;
  };
}

// Backed by `GET /admin/api/v1/rdp/:id` on the Go server (WASM/agent
// checks, node lookup, ephemeral pre-auth key mint).
export async function clientLoader({ params, request }: Route.ClientLoaderArgs) {
  const search = new URL(request.url).search;
  const res = await apiFetch(`/rdp/${encodeURIComponent(params.id)}${search}`);
  if (res.status === 401) {
    throw redirect("/login");
  }
  const body = await res.json().catch(() => null);
  if (!res.ok) {
    throw data(body ?? { title: "RDP Not Available", message: "hp_rdp.wasm is not available on this server." }, res.status);
  }
  return body as RDPLoaderData;
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
