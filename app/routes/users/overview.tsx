import PageError from "~/components/page-error";
import { apiAction, apiGet } from "~/lib/api";
import type { Machine, User } from "~/types";
import cn from "~/utils/cn";

import type { Route } from "./+types/overview";
import HeadplaneUserRow from "./components/headplane-user-row";
import HeadscaleUserRow from "./components/headscale-user-row";
import ManageBanner from "./components/manage-banner";

// Client-side copy of the server `Role` union (`keyof typeof Roles` in
// ~/server/web/roles). This module must not import server-only code.
export type Role =
  | "owner"
  | "admin"
  | "network_admin"
  | "it_admin"
  | "auditor"
  | "viewer"
  | "member";

export interface HeadplaneUserData {
  id: string;
  sub: string;
  name: string | null;
  email: string | null;
  role: Role;
  headscaleUserId: string | null;
  createdAt: string | null;
  lastLoginAt: string | null;
  // Enriched from Headscale API (may be absent if API failed)
  linkedHeadscaleUser?: User;
  machines: Machine[];
  profilePicUrl?: string;
}

export interface UnlinkedHeadscaleUser extends User {
  machines: Machine[];
}

// Shape of GET /admin/api/v1/users (see the Phase 0 routes contract).
interface UsersData {
  writable: boolean;
  currentUserId: string | null;
  isOwner: boolean;
  oidc: { issuer: string } | null;
  magic: string | null;
  apiError: string | null;
  headplaneUsers: HeadplaneUserData[];
  unlinkedHeadscaleUsers: UnlinkedHeadscaleUser[];
  headscaleUsersForLink: { id: string; name: string; claimed: boolean }[];
}

export async function clientLoader(): Promise<UsersData> {
  // Auth gates stay server-side: apiGet throws redirect("/login") on 401 and
  // Error (→ ErrorBoundary) on other failures, mirroring the old loader.
  return apiGet<UsersData>("/users");
}

export async function clientAction({ request }: Route.ClientActionArgs) {
  return apiAction("/users/actions", await request.formData());
}

export default function Page({ loaderData }: Route.ComponentProps) {
  return (
    <>
      <h1 className="mb-1.5 text-2xl font-medium">Users</h1>
      <p className="text-md mb-8">Manage the users in your network and their permissions.</p>
      <ManageBanner isDisabled={!loaderData.writable} oidc={loaderData.oidc ?? undefined} />

      {loaderData.apiError && (
        <div
          className={cn(
            "mb-6 flex items-start gap-3 rounded-lg border p-4",
            "border-red-200 bg-red-50 text-red-800",
            "dark:border-red-800 dark:bg-red-950 dark:text-red-200",
          )}
        >
          <p className="text-sm">{loaderData.apiError}</p>
        </div>
      )}

      <section>
        <h2 className="mb-3 text-lg font-medium">Headplane Users</h2>
        {loaderData.headplaneUsers.length === 0 ? (
          <p className="text-sm text-mist-600 dark:text-mist-300">
            No users have signed into Headplane yet.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[640px] table-auto rounded-lg">
              <thead className="text-mist-600 dark:text-mist-300">
                <tr className="px-0.5 text-left">
                  <th className="pb-2 text-xs font-bold uppercase">User</th>
                  <th className="pb-2 text-xs font-bold uppercase">Role</th>
                  <th className="pb-2 text-xs font-bold uppercase">Last Login</th>
                  <th className="pb-2 text-xs font-bold uppercase">Status</th>
                  <th className="w-12 pb-2">
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody
                className={cn(
                  "divide-y divide-mist-100 dark:divide-mist-800 align-top",
                  "border-t border-mist-100 dark:border-mist-800",
                )}
              >
                {loaderData.headplaneUsers.map((user) => (
                  <HeadplaneUserRow
                    isSelf={user.id === loaderData.currentUserId}
                    isOwner={loaderData.isOwner}
                    key={user.id}
                    headscaleUsers={loaderData.headscaleUsersForLink}
                    user={user}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {!loaderData.apiError && loaderData.unlinkedHeadscaleUsers.length > 0 && (
        <section className="mt-10">
          <h2 className="mb-1 text-lg font-medium">Unlinked Headscale Users</h2>
          <p className="mb-3 text-sm text-mist-600 dark:text-mist-300">
            These Headscale users are not linked to a Headplane account and cannot be managed
            through Headplane.
          </p>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[640px] table-auto rounded-lg">
              <thead className="text-mist-600 dark:text-mist-300">
                <tr className="px-0.5 text-left">
                  <th className="pb-2 text-xs font-bold uppercase">User</th>
                  <th className="pb-2 text-xs font-bold uppercase">Created At</th>
                  <th className="pb-2 text-xs font-bold uppercase">Status</th>
                  <th className="w-12 pb-2">
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody
                className={cn(
                  "divide-y divide-mist-100 dark:divide-mist-800 align-top",
                  "border-t border-mist-100 dark:border-mist-800",
                )}
              >
                {loaderData.unlinkedHeadscaleUsers.map((user) => (
                  <HeadscaleUserRow key={user.id} user={user} writable={loaderData.writable} />
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </>
  );
}

export function ErrorBoundary({ error }: Route.ErrorBoundaryProps) {
  return <PageError error={error} page="Users" />;
}
