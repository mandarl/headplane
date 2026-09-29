import { Loader2, WifiOff } from "lucide-react";
import { useEffect, useState } from "react";
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
import { isSSHError, SSHErrorBoundary, sshErrors } from "./errors";
import Ghostty from "./ghostty.client";
import UserPrompt from "./user-prompt";
import type { HeadplaneSSH } from "./wasm.client";
import { loadHeadplaneWASM } from "./wasm.client";

const WASM_MODULE_URL = `${__PREFIX__}/hp_ssh.wasm`;

export const shouldRevalidate: ShouldRevalidateFunction = ({ currentUrl, nextUrl }) => {
  // Only revalidate when transitioning from no-user to user (UserPrompt → SSHConsole).
  // Returning false for everything else prevents the loader from re-running
  // mid-session, which would generate a new pre-auth key and reset the connection.
  return !currentUrl.searchParams.has("user") && nextUrl.searchParams.has("user");
};

interface SSHLoaderData {
  hostname: string;
  username?: string;
  offline: boolean;
  isWindows: boolean;
  node?: {
    ipAddress: string;
    controlURL: string;
    preAuthKey: string;
    ephemeralHostname: string;
  };
}

// The Go server (`GET /admin/api/v1/ssh/:id`) does the WASM/agent checks,
// node lookup, Headscale-user resolution, and the 5-minute ephemeral
// pre-auth key mint. Non-2xx responses carry the `{title,message,anchor}`
// body the ErrorBoundary renders.
export async function clientLoader({ params, request }: Route.ClientLoaderArgs) {
  const search = new URL(request.url).search;
  const res = await apiFetch(`/ssh/${encodeURIComponent(params.id)}${search}`);
  if (res.status === 401) {
    throw redirect("/login");
  }
  const body = await res.json().catch(() => null);
  if (!res.ok) {
    throw data(body ?? sshErrors.wasm_missing, res.status);
  }
  return body as SSHLoaderData;
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
  const { hostname, username, offline, node, isWindows } = loaderData;
  const location = useLocation();
  const password: string | undefined = (location.state as { password?: string } | null)?.password;

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
    return <UserPrompt hostname={hostname} isWindows={isWindows} />;
  }

  return <SSHConsole hostname={hostname} username={username} password={password} node={node} />;
}

function SSHConsole({
  hostname,
  username,
  password,
  node,
}: {
  hostname: string;
  username: string;
  password?: string;
  node: { ipAddress: string; controlURL: string; preAuthKey: string; ephemeralHostname: string };
}) {
  const [ssh, setSsh] = useState<HeadplaneSSH | null>(null);
  const [connected, setConnected] = useState(false);
  const [status, setStatus] = useState("Starting tunnel…");

  useEffect(() => {
    let cancelled = false;

    console.log("[ssh] Loading WASM factory");
    loadHeadplaneWASM().then((create) => {
      console.log("[ssh] Factory loaded, creating IPN", create);

      if (cancelled) {
        return;
      }

      setStatus("Joining Tailnet…");
      const instance = create({
        controlURL: node.controlURL,
        preAuthKey: node.preAuthKey,
        hostname: node.ephemeralHostname,
        onReady: () => {
          console.log("[ssh] IPN ready (Running)");
          if (!cancelled) {
            setStatus(`Connecting to ${hostname}…`);
            setSsh(instance);
          }
        },
        onError: (msg) => console.error("[ssh] IPN error:", msg),
      });

      console.log("[ssh] IPN instance created", instance);
    });

    return () => {
      cancelled = true;
    };
  }, [node]);

  return (
    <div className="fixed inset-0 flex flex-col bg-black">
      {!connected && (
        <div className="absolute inset-0 z-50 flex items-center justify-center">
          <div className="flex flex-col items-center gap-3">
            <Loader2 className="size-8 animate-spin text-mist-200" />
            <p className="text-sm text-mist-400">{status}</p>
          </div>
        </div>
      )}

      {ssh && (
        <Ghostty
          ssh={ssh}
          username={username}
          password={password}
          ipAddress={node.ipAddress}
          onConnected={() => setConnected(true)}
        />
      )}
    </div>
  );
}

export function ErrorBoundary({ error }: Route.ErrorBoundaryProps) {
  const routeError = isRouteErrorResponse(error) ? error.data : null;
  if (routeError == null || !isSSHError(routeError)) {
    // Pass through further down the tree to the global error boundary
    throw error;
  }

  return (
    <div className="flex h-screen w-screen items-center justify-center">
      <SSHErrorBoundary
        title={routeError.title}
        message={routeError.message}
        anchor={routeError.anchor}
      />
    </div>
  );
}
