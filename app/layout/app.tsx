import { Outlet, type ShouldRevalidateFunction } from "react-router";

import { ErrorBanner } from "~/components/error-banner";
import StatusBanner from "~/components/status-banner";
import { apiGet } from "~/lib/api";

import type { Route } from "./+types/app";
import Footer from "./footer";
import Header from "./header";

export const shouldRevalidate: ShouldRevalidateFunction = ({
  currentUrl,
  nextUrl,
  formAction,
  defaultShouldRevalidate,
}) => {
  if (formAction) {
    return defaultShouldRevalidate;
  }

  // Allow programmatic revalidations (e.g. SSE-triggered) where the URL hasn't changed
  if (currentUrl.href === nextUrl.href) {
    return defaultShouldRevalidate;
  }

  return false;
};

export interface BootUser {
  kind: "oidc" | "api_key";
  subject: string;
  name: string;
  email?: string;
  username?: string;
  picture?: string;
  headscaleUserId?: string | null;
  role?: string;
}

export interface BootData {
  user: BootUser;
  access: {
    dns: boolean;
    machines: boolean;
    policy: boolean;
    settings: boolean;
    ui: boolean;
    users: boolean;
  };
  baseUrl: string;
  configAvailable: boolean;
  isHealthy: boolean;
  isDebug: boolean;
}

export async function clientLoader(): Promise<BootData> {
  // 401 → apiGet throws redirect("/login"), mirroring the old server loader
  // (which also destroyed the session and re-validated the Headscale key).
  return apiGet<BootData>("/boot");
}

export default function AppLayout({ loaderData }: Route.ComponentProps) {
  return (
    <>
      <Header
        access={loaderData.access}
        configAvailable={loaderData.configAvailable}
        user={loaderData.user}
      />
      <main className="container mt-4 mb-24 overscroll-contain">
        {!loaderData.isHealthy && (
          <StatusBanner
            className="mb-4"
            dismissable={false}
            title="Headscale Unreachable"
            variant="critical"
          >
            Unable to connect to the Headscale server. Data shown may be stale and changes cannot be
            saved until the connection is restored.
          </StatusBanner>
        )}
        <Outlet />
      </main>
      <Footer isDebug={loaderData.isDebug} baseUrl={loaderData.baseUrl} />
    </>
  );
}

export function ErrorBoundary({ error }: Route.ErrorBoundaryProps) {
  return (
    <div className="mx-auto my-24 w-fit overscroll-contain">
      <ErrorBanner className="max-w-2xl" error={error} />
    </div>
  );
}
