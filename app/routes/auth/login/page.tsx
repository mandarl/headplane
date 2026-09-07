import { AlertCircle } from "lucide-react";
import { useEffect, useState } from "react";
import {
  Form,
  Link as RouterLink,
  redirect,
  redirectDocument,
  UNSAFE_decodeViaTurboStream as decodeViaTurboStream,
  useSearchParams,
} from "react-router";

import Button from "~/components/button";
import Card from "~/components/card";
import Code from "~/components/code";
import Input from "~/components/input";
import Link from "~/components/link";
import type { OidcErrorCode } from "~/server/oidc/provider";
import { useLiveData } from "~/utils/live-data";

import type { Route } from "./+types/page";
import { OidcConfigErrorNotice, OidcDiscoveryFailedNotice } from "./config-error";
import Logout from "./logout";
import { OidcErrorNotice } from "./oidc-error";

// The server-driven login action, re-exported through an indirection so the
// SPA build's route-module validator (which scans static export names for
// server-only exports) does not see it. See login-action.ts.
export * from "./login-action";

interface LoginConfig {
  oidcEnabled: boolean;
  oidcErrorCodes: OidcErrorCode[];
  cookieSecure: boolean;
  disableApiKeyLogin: boolean;
}

export async function clientLoader({ request }: Route.ClientLoaderArgs) {
  // The v1 boot endpoint is public for its `login` section: on 401 it still
  // returns the login configuration (mirrors the old server loader, which
  // ran without a session on this page).
  const res = await fetch(`${__PREFIX__}/api/v1/boot`, { credentials: "include" });
  if (res.ok) {
    // Already authenticated — the old loader redirected to /machines.
    throw redirect("/machines");
  }
  const body = (await res.json().catch(() => null)) as { login?: LoginConfig } | null;
  const login: LoginConfig = body?.login ?? {
    oidcEnabled: false,
    oidcErrorCodes: [],
    cookieSecure: false,
    disableApiKeyLogin: false,
  };

  const qp = new URL(request.url).searchParams;
  const urlState = qp.get("s") ?? undefined;

  if (login.oidcEnabled && login.disableApiKeyLogin && urlState !== "logout") {
    // Server-driven OIDC flow — needs a full document navigation, not a
    // client-side route change (there is no /oidc/start SPA route).
    throw redirectDocument(`${__PREFIX__}/oidc/start`);
  }

  return {
    isCookieSecureEnabled: login.cookieSecure,
    isOidcConnectorEnabled: login.oidcEnabled,
    oidcErrorCodes: login.oidcErrorCodes,
    urlState,
  };
}

export async function clientAction({ request }: Route.ClientActionArgs) {
  // Forward the credentials to the server-driven login action as a React
  // Router single-fetch data request (`.data`). A plain document POST would
  // have the action result swallowed into a re-rendered page (the old SSR
  // behavior); the data request returns it as turbo-stream JSON instead.
  // The interim server proxies this to the Node SSR server (later: the Go
  // server, whose POST /login will speak plain JSON and let this go back to
  // a simple fetch).
  const res = await fetch(`${__PREFIX__}/login.data`, {
    method: "POST",
    body: await request.formData(),
    credentials: "include",
  });
  if (!res.body) {
    throw new Error("Login failed with an unexpected response");
  }
  const { value } = await decodeViaTurboStream(res.body, window);
  if (value !== null && typeof value === "object" && "redirect" in value) {
    // Success: the server issued a redirect (session cookie is set) —
    // continue client-side so the SPA boots authenticated.
    const to = (value as { redirect?: unknown }).redirect;
    throw redirect(typeof to === "string" ? to : "/machines");
  }
  const data = (value as { data?: unknown } | null)?.data as
    | { success?: boolean; message?: string }
    | undefined;
  if (data && data.success === false) {
    return data;
  }
  throw new Error("Login failed with an unexpected response");
}

export default function Page({ loaderData, actionData }: Route.ComponentProps) {
  const { isCookieSecureEnabled, isOidcConnectorEnabled, oidcErrorCodes, urlState } = loaderData;

  const [showCookieWarning, setShowCookieWarning] = useState(false);
  const [params] = useSearchParams();
  const { pause } = useLiveData();

  useEffect(() => {
    // This page does NOT need stale while revalidate logic
    pause();

    if (isCookieSecureEnabled && window.location.protocol !== "https:") {
      setShowCookieWarning(true);
    }
  });

  useEffect(() => {
    // State is a one time thing, we need to remove it after it has
    // Been consumed to prevent logic loops.
    if (urlState !== null) {
      const searchParams = new URLSearchParams(params);
      searchParams.delete("s");

      // Replacing because it's not a navigation, just a cleanup of the URL
      // We can't use the useSearchParams method since it revalidates
      // Which will trigger a full reload
      const newUrl = searchParams.toString()
        ? `{${window.location.pathname}?${searchParams.toString()}`
        : window.location.pathname;

      window.history.replaceState(null, "", newUrl);
    }
  }, [urlState, params]);

  if (urlState === "logout") {
    return <Logout />;
  }

  return (
    <div className="flex h-screen w-screen items-center justify-center">
      <div>
        {urlState?.startsWith("error_") ? (
          <OidcErrorNotice code={urlState} />
        ) : oidcErrorCodes.includes("discovery_failed") ? (
          <OidcDiscoveryFailedNotice />
        ) : oidcErrorCodes.length > 0 ? (
          <OidcConfigErrorNotice errors={oidcErrorCodes} />
        ) : showCookieWarning ? (
          <Card className="m-4 mb-4 max-w-md border border-red-500 sm:m-0 sm:mb-4">
            <div className="flex items-center justify-between gap-4">
              <Card.Title className="text-red-500">Configuration Issue</Card.Title>
              <AlertCircle className="mb-2 h-6 w-6 text-red-500" />
            </div>
            {showCookieWarning ? (
              <Card.Text className="text-sm">
                Headplane is configured to use secure cookies, but this site is being served over an
                insecure connection and login will not work correctly.{" "}
                <Link
                  external
                  styled
                  to="https://headplane.net/configuration/common-issues#issue-logging-in-does-not-do-anything"
                >
                  Learn more.
                </Link>
              </Card.Text>
            ) : undefined}
          </Card>
        ) : undefined}
        <Card className="m-4 max-w-md sm:m-0">
          <Card.Title>Welcome to Headplane</Card.Title>
          <Form method="POST">
            <Card.Text>
              Enter an API key to authenticate with Headplane. You can generate one by running{" "}
              <Code>headscale apikeys create</Code> in your terminal.
            </Card.Text>
            <Input
              className="mt-8 mb-2"
              required
              label="API Key"
              labelHidden
              name="api_key"
              placeholder="API Key"
              type="password"
            />
            {actionData?.success === false ? (
              <Card.Text className="mb-2 text-sm text-red-600 dark:text-red-300">
                {actionData.message}
              </Card.Text>
            ) : undefined}
            <Button className="w-full" type="submit" variant="heavy">
              Sign In
            </Button>
          </Form>
          {isOidcConnectorEnabled ? (
            <RouterLink to="/oidc/start" prefetch="none" reloadDocument>
              <Button className="mt-2 w-full" disabled={oidcErrorCodes.length > 0} variant="light">
                Single Sign-On
              </Button>
            </RouterLink>
          ) : undefined}
        </Card>
      </div>
    </div>
  );
}
