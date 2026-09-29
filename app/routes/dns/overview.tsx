import { useLoaderData } from "react-router";

import Code from "~/components/code";
import Notice from "~/components/notice";
import PageError from "~/components/page-error";
import { apiAction, apiGet } from "~/lib/api";

import type { Route } from "./+types/overview";
import ManageDomains from "./components/manage-domains";
import ManageNS from "./components/manage-ns";
import ManageRecords from "./components/manage-records";
import RenameTailnet from "./components/rename-tailnet";
import ToggleMagic from "./components/toggle-magic";

// Shape of GET /admin/api/v1/dns (see the Phase 0 routes contract).
interface DnsData {
  prefixes: string[];
  magicDns: boolean;
  baseDomain: string;
  nameservers: string[];
  splitDns: Record<string, string[]>;
  searchDomains: string[];
  overrideDns: boolean;
  extraRecords: { name: string; type: "A" | "AAAA"; value: string }[];
  access: boolean;
  writable: boolean;
}

export async function clientLoader(): Promise<DnsData> {
  // Auth gates stay server-side: apiGet throws redirect("/login") on 401 and
  // Error (→ ErrorBoundary) on other failures, mirroring the old loader.
  return apiGet<DnsData>("/dns");
}

export async function clientAction({ request }: Route.ClientActionArgs) {
  return apiAction("/dns/actions", await request.formData());
}

export default function Page() {
  const data = useLoaderData<typeof clientLoader>();

  const allNs: Record<string, string[]> = {};
  for (const key of Object.keys(data.splitDns)) {
    allNs[key] = data.splitDns[key];
  }

  allNs.global = data.nameservers;
  const isDisabled = data.access === false || data.writable === false;

  return (
    <div className="flex max-w-(--breakpoint-lg) flex-col gap-16">
      {data.writable ? undefined : (
        <Notice>
          The Headscale configuration is read-only. You cannot make changes to the configuration
        </Notice>
      )}
      {data.access ? undefined : (
        <Notice>
          Your permissions do not allow you to modify the DNS settings for this tailnet.
        </Notice>
      )}
      <RenameTailnet isDisabled={isDisabled} name={data.baseDomain} />
      <ManageNS isDisabled={isDisabled} nameservers={allNs} overrideLocalDns={data.overrideDns} />
      <ManageRecords isDisabled={isDisabled} records={data.extraRecords} />
      <ManageDomains
        isDisabled={isDisabled}
        magic={data.magicDns ? data.baseDomain : undefined}
        searchDomains={data.searchDomains}
      />

      <div className="flex w-full flex-col sm:w-2/3">
        <h1 className="mb-4 text-2xl font-medium">Magic DNS</h1>
        <p className="mb-4">
          Automatically register domain names for each device on the tailnet. Devices will be
          accessible at{" "}
          <Code>
            [device].
            {data.baseDomain}
          </Code>{" "}
          when Magic DNS is enabled.
        </p>
        <ToggleMagic isDisabled={isDisabled} isEnabled={data.magicDns} />
      </div>
    </div>
  );
}

export function ErrorBoundary({ error }: { error: unknown }) {
  return <PageError error={error} page="DNS" />;
}
