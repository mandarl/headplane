import { ChevronDown, ChevronUp, Info, X } from "lucide-react";
import { useMemo, useState } from "react";
import { useSearchParams } from "react-router";

import Code from "~/components/code";
import Input from "~/components/input";
import Link from "~/components/link";
import PageError from "~/components/page-error";
import Tooltip from "~/components/tooltip";
import { apiAction, apiGet } from "~/lib/api";
import type { Machine, User } from "~/types";
import cn from "~/utils/cn";
import { sortNodeTags, type PopulatedNode } from "~/utils/node-info";

import type { Route } from "./+types/overview";
import { MachineFilters } from "./components/machine-filters";
import MachineRow from "./components/machine-row";
import NewMachine from "./dialogs/new";
import { useMachineFilterParams } from "./hooks/use-machine-filter-params";

type MachinesOverviewData = {
  agent: {
    syncedAt: string | null;
    nodeCount: number;
    nodeKey?: string;
  } | null;
  headscaleUserId: string | null;
  magic: string | null;
  nodes: Machine[];
  populatedNodes: PopulatedNode[];
  preAuth: boolean;
  publicServer: string | null;
  rdpGatewayEnabled: boolean;
  server: string;
  supportsNodeOwnerChange: boolean;
  users: User[];
  writable: boolean;
};

export async function clientLoader() {
  const data = await apiGet<MachinesOverviewData>("/machines");
  // JSON has no `undefined`: the wire contract uses `null` where the old
  // server loader returned `undefined` (agent, headscaleUserId, magic,
  // publicServer). Convert back so the unchanged components below behave
  // exactly as before.
  return {
    ...data,
    agent: data.agent ?? undefined,
    headscaleUserId: data.headscaleUserId ?? undefined,
    magic: data.magic ?? undefined,
    publicServer: data.publicServer ?? undefined,
  };
}

export async function clientAction({ request }: Route.ClientActionArgs) {
  return apiAction("/machines/actions", await request.formData());
}

type SortField = "name" | "ip" | "version" | "lastSeen";

const STATUS_MATCH: Record<string, (n: PopulatedNode) => boolean> = {
  online: (n) => n.online && !n.expired,
  offline: (n) => !n.online && !n.expired,
  expired: (n) => n.expired,
};

const ROUTE_MATCH: Record<string, (n: PopulatedNode) => boolean> = {
  "exit-node": (n) => n.customRouting.exitRoutes.length > 0,
  subnet: (n) =>
    n.customRouting.subnetApprovedRoutes.length > 0 ||
    n.customRouting.subnetWaitingRoutes.length > 0,
};

export default function Page({ loaderData }: Route.ComponentProps) {
  const [searchParams, setSearchParams] = useSearchParams();
  const [sortField, setSortField] = useState<SortField>("name");
  const [sortDirection, setSortDirection] = useState<"asc" | "desc">("asc");

  const searchQuery = searchParams.get("q") ?? "";
  const { filterUser, filterTag, filterStatus, filterRoute, hasActiveFilters } =
    useMachineFilterParams();

  const setSearchQuery = (value: string) => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      const v = value.slice(0, 100);
      if (v) next.set("q", v);
      else next.delete("q");
      return next;
    });
  };

  const clearSearch = () => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      next.delete("q");
      return next;
    });
  };

  const filteredAndSortedNodes = useMemo(() => {
    const query = searchQuery.toLowerCase().trim();

    let nodes = loaderData.populatedNodes.filter(
      (node) =>
        (!query ||
          node.givenName.toLowerCase().includes(query) ||
          node.ipAddresses.some((ip) => ip.toLowerCase().includes(query))) &&
        (filterUser === null ||
          (filterUser === "tag-owned" ? !node.user : node.user?.name === filterUser)) &&
        (filterTag === null || (node.tags?.includes(filterTag) ?? false)) &&
        (filterStatus === null || STATUS_MATCH[filterStatus](node)) &&
        (filterRoute === null || ROUTE_MATCH[filterRoute](node)),
    );

    nodes = [...nodes].toSorted((a, b) => {
      let comparison = 0;

      switch (sortField) {
        case "name": {
          comparison = a.givenName.localeCompare(b.givenName);
          break;
        }
        case "ip": {
          const getIPv4 = (addresses: string[]) =>
            addresses.find((ip) => !ip.includes(":")) || addresses[0] || "";
          const ipA = getIPv4(a.ipAddresses);
          const ipB = getIPv4(b.ipAddresses);

          if (!ipA.includes(":") && !ipB.includes(":")) {
            const octetsA = ipA.split(".").map(Number);
            const octetsB = ipB.split(".").map(Number);
            for (let i = 0; i < 4; i++) {
              if (octetsA[i] !== octetsB[i]) {
                comparison = octetsA[i] - octetsB[i];
                break;
              }
            }
          } else {
            comparison = ipA.localeCompare(ipB);
          }
          break;
        }
        case "version": {
          const versionA = a.hostInfo?.IPNVersion?.split("-")[0] || "0";
          const versionB = b.hostInfo?.IPNVersion?.split("-")[0] || "0";
          const partsA = versionA.split(".").map(Number);
          const partsB = versionB.split(".").map(Number);
          const maxLen = Math.max(partsA.length, partsB.length);

          for (let i = 0; i < maxLen; i++) {
            const segA = partsA[i] || 0;
            const segB = partsB[i] || 0;
            if (segA !== segB) {
              comparison = segA - segB;
              break;
            }
          }
          break;
        }
        case "lastSeen": {
          if (a.online !== b.online) {
            comparison = a.online ? 1 : -1;
            break;
          }
          comparison = new Date(a.lastSeen).getTime() - new Date(b.lastSeen).getTime();
          break;
        }
      }

      return sortDirection === "asc" ? comparison : -comparison;
    });

    return nodes;
  }, [
    loaderData.populatedNodes,
    searchQuery,
    filterUser,
    filterTag,
    filterStatus,
    filterRoute,
    sortField,
    sortDirection,
  ]);

  const handleSort = (field: SortField) => {
    if (sortField === field) {
      setSortDirection((prev) => (prev === "asc" ? "desc" : "asc"));
    } else {
      setSortField(field);
      setSortDirection("asc");
    }
  };

  return (
    <>
      <div className="mb-6 flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex flex-col">
          <h1 className="mb-2 text-2xl font-medium">Machines</h1>
          <p>
            Manage the devices connected to your Tailnet.{" "}
            <Link external styled to="https://tailscale.com/kb/1372/manage-devices">
              Learn more
            </Link>
          </p>
        </div>
        <NewMachine
          disabledKeys={loaderData.preAuth ? [] : ["pre-auth"]}
          isDisabled={!loaderData.writable}
          server={loaderData.publicServer ?? loaderData.server}
          users={loaderData.users}
        />
      </div>
      <div className="mb-4 flex flex-wrap items-center gap-3">
        <div className="relative w-64">
          <Input
            label="Search machines"
            labelHidden
            maxLength={100}
            onChange={setSearchQuery}
            placeholder="Search by name or IP address..."
            value={searchQuery}
          />
          {searchQuery && (
            <button
              aria-label="Clear search"
              className={cn(
                "absolute right-2 top-1/2 -translate-y-1/2",
                "p-1 rounded-full",
                "text-mist-400 hover:text-mist-600",
                "dark:text-mist-500 dark:hover:text-mist-300",
                "hover:bg-mist-100 dark:hover:bg-mist-800",
              )}
              onClick={clearSearch}
              type="button"
            >
              <X className="h-4 w-4" />
            </button>
          )}
        </div>
        <MachineFilters users={loaderData.users} populatedNodes={loaderData.populatedNodes} />
        <span className="ml-auto text-sm whitespace-nowrap text-mist-500">
          {searchQuery || hasActiveFilters
            ? `Showing ${filteredAndSortedNodes.length} of ${loaderData.populatedNodes.length} machines`
            : `${loaderData.populatedNodes.length} machines`}
        </span>
      </div>
      <div className="overflow-x-auto">
        <table className="w-full min-w-160 table-auto rounded-lg">
          <thead className="text-mist-600 dark:text-mist-300">
            <tr className="px-0.5 text-left">
              <th
                aria-sort={
                  sortField === "name"
                    ? sortDirection === "asc"
                      ? "ascending"
                      : "descending"
                    : "none"
                }
                className="pb-2 text-xs font-bold uppercase"
              >
                <button
                  aria-label="Sort by name"
                  className={cn(
                    "flex items-center gap-x-1 cursor-pointer",
                    "hover:text-mist-900 dark:hover:text-mist-100",
                  )}
                  onClick={() => handleSort("name")}
                  type="button"
                >
                  Name
                  {sortField === "name" &&
                    (sortDirection === "asc" ? (
                      <ChevronUp className="h-3 w-3" />
                    ) : (
                      <ChevronDown className="h-3 w-3" />
                    ))}
                </button>
              </th>
              <th
                aria-sort={
                  sortField === "ip"
                    ? sortDirection === "asc"
                      ? "ascending"
                      : "descending"
                    : "none"
                }
                className="w-1/4 pb-2"
              >
                <div className="flex items-center gap-x-1">
                  <button
                    aria-label="Sort by IP address"
                    className={cn(
                      "flex items-center gap-x-1 cursor-pointer uppercase text-xs font-bold",
                      "hover:text-mist-900 dark:hover:text-mist-100",
                    )}
                    onClick={() => handleSort("ip")}
                    type="button"
                  >
                    Addresses
                    {sortField === "ip" &&
                      (sortDirection === "asc" ? (
                        <ChevronUp className="h-3 w-3" />
                      ) : (
                        <ChevronDown className="h-3 w-3" />
                      ))}
                  </button>
                  {loaderData.magic ? (
                    <Tooltip
                      content={
                        <span className="font-normal">
                          Since MagicDNS is enabled, you can access devices based on their name and
                          also at{" "}
                          <Code>
                            [name].
                            {loaderData.magic}
                          </Code>
                        </span>
                      }
                    >
                      <Info className="h-4 w-4" />
                    </Tooltip>
                  ) : undefined}
                </div>
              </th>
              {/* We only want to show the version column if there are agents */}
              {loaderData.agent !== undefined ? (
                <th
                  aria-sort={
                    sortField === "version"
                      ? sortDirection === "asc"
                        ? "ascending"
                        : "descending"
                      : "none"
                  }
                  className="pb-2 text-xs font-bold uppercase"
                >
                  <button
                    aria-label="Sort by version"
                    className={cn(
                      "flex items-center gap-x-1 cursor-pointer",
                      "hover:text-mist-900 dark:hover:text-mist-100",
                    )}
                    onClick={() => handleSort("version")}
                    type="button"
                  >
                    Version
                    {sortField === "version" &&
                      (sortDirection === "asc" ? (
                        <ChevronUp className="h-3 w-3" />
                      ) : (
                        <ChevronDown className="h-3 w-3" />
                      ))}
                  </button>
                </th>
              ) : undefined}
              <th
                aria-sort={
                  sortField === "lastSeen"
                    ? sortDirection === "asc"
                      ? "ascending"
                      : "descending"
                    : "none"
                }
                className="pb-2 text-xs font-bold uppercase"
              >
                <button
                  aria-label="Sort by last seen"
                  className={cn(
                    "flex items-center gap-x-1 cursor-pointer",
                    "hover:text-mist-900 dark:hover:text-mist-100",
                  )}
                  onClick={() => handleSort("lastSeen")}
                  type="button"
                >
                  Last Seen
                  {sortField === "lastSeen" &&
                    (sortDirection === "asc" ? (
                      <ChevronUp className="h-3 w-3" />
                    ) : (
                      <ChevronDown className="h-3 w-3" />
                    ))}
                </button>
              </th>
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
            {filteredAndSortedNodes.length === 0 ? (
              <tr>
                <td
                  className="py-8 text-center text-mist-500"
                  colSpan={loaderData.agent !== undefined ? 6 : 5}
                >
                  No machines match the current filters
                </td>
              </tr>
            ) : (
              filteredAndSortedNodes.map((node) => (
                <MachineRow
                  existingTags={sortNodeTags(loaderData.nodes)}
                  isAgent={
                    loaderData.agent !== undefined
                      ? node.nodeKey === loaderData.agent.nodeKey
                      : undefined
                  }
                  isDisabled={
                    loaderData.writable
                      ? false // If the user has write permissions, they can edit all machines
                      : node.user?.id !== loaderData.headscaleUserId
                  }
                  key={node.id}
                  magic={loaderData.magic}
                  node={node}
                  rdpGatewayEnabled={loaderData.rdpGatewayEnabled}
                  users={loaderData.users}
                  supportsNodeOwnerChange={loaderData.supportsNodeOwnerChange}
                />
              ))
            )}
          </tbody>
        </table>
      </div>
    </>
  );
}

export function ErrorBoundary({ error }: Route.ErrorBoundaryProps) {
  return <PageError error={error} page="Machines" />;
}
