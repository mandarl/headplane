import { AlertCircle, Construction, Eye, FlaskConical, Pencil } from "lucide-react";
import { Suspense, lazy, useEffect, useState } from "react";
import { isRouteErrorResponse, useFetcher, useRevalidator } from "react-router";

import Button from "~/components/button";
import Card from "~/components/card";
import Code from "~/components/code";
import Link from "~/components/link";
import Notice from "~/components/notice";
import PageError from "~/components/page-error";
import { Tabs, TabsList, TabsPanel, TabsTab } from "~/components/tabs";
import { apiAction, apiGet } from "~/lib/api";
import toast from "~/utils/toast";

import type { Route } from "./+types/overview";
import Fallback from "./components/fallback";

const LazyEditor = lazy(() =>
  import("./components/cm.client").then((m) => ({ default: m.Editor })),
);
const LazyDiffer = lazy(() =>
  import("./components/cm.client").then((m) => ({ default: m.Differ })),
);

// Shape of GET /admin/api/v1/acls (see the Phase 0 routes contract).
interface AclData {
  access: boolean;
  writable: boolean;
  policy: string;
}

export async function clientLoader(): Promise<AclData> {
  // Auth gates stay server-side: apiGet throws redirect("/login") on 401 and
  // Error (→ ErrorBoundary) on other failures, mirroring the old loader.
  return apiGet<AclData>("/acls");
}

// Response shapes of PATCH /admin/api/v1/acls (see the Phase 0 routes contract).
type AclActionResult =
  | { success: true; error?: undefined; policy: string; updatedAt: string | null }
  | { success: false; error: string; policy: null; updatedAt: null };

export async function clientAction({ request }: Route.ClientActionArgs) {
  // The UI submits with `fetcher.submit(form, { method: "PATCH" })`; apiAction
  // forwards the PATCH and mirrors RR action `data()` semantics for useActionData.
  const result = await apiAction<AclActionResult>("/acls", await request.formData(), "PATCH");
  if (result.success) {
    // The JSON contract returns `error: null` on success, but the old action
    // returned `error: undefined` (dropped by turbo-stream serialization) and
    // the component checks `error !== undefined`. Normalize so the error
    // Notice doesn't render (and `.split` never runs on null).
    result.error = undefined;
  }
  return result;
}

export default function Page({ loaderData: { access, writable, policy } }: Route.ComponentProps) {
  const [codePolicy, setCodePolicy] = useState(policy);
  const fetcher = useFetcher<typeof clientAction>();
  const { revalidate } = useRevalidator();
  const disabled = !access || !writable; // Disable if no permission or not writable

  useEffect(() => {
    // Update the codePolicy when the loader data changes
    if (policy !== codePolicy) {
      setCodePolicy(policy);
    }
  }, [policy]);

  useEffect(() => {
    if (!fetcher.data) {
      // No data yet, return
      return;
    }

    if (fetcher.data.success === true) {
      toast("Updated policy");
      revalidate();
    }
  }, [fetcher.data]);

  return (
    <div>
      {!access ? (
        <Notice title="ACL Policy restricted" variant="warning">
          You do not have the necessary permissions to edit the Access Control List policy. Please
          contact your administrator to request access or to make changes to the ACL policy.
        </Notice>
      ) : !writable ? (
        <Notice title="Read-only ACL Policy" variant="error">
          The ACL policy mode is most likely set to <Code>file</Code> in your Headscale
          configuration. This means that the ACL file cannot be edited through the web interface. In
          order to resolve this, you'll need to set <Code>policy.mode</Code> to{" "}
          <Code>database</Code> in your Headscale configuration.
        </Notice>
      ) : undefined}
      <h1 className="mb-4 text-2xl font-medium">Access Control List (ACL)</h1>
      <p className="mb-4 max-w-prose">
        The ACL file is used to define the access control rules for your network. You can find more
        information about the ACL file in the{" "}
        <Link external styled to="https://tailscale.com/kb/1018/acls">
          Tailscale ACL guide
        </Link>{" "}
        and the{" "}
        <Link external styled to="https://headscale.net/stable/ref/acls/">
          Headscale docs
        </Link>
        .
      </p>
      {fetcher.data?.error !== undefined ? (
        <Notice title={fetcher.data.error.split(":")[0] ?? "Error"} variant="error">
          {fetcher.data.error.split(":").slice(1).join(": ") ??
            "An unknown error occurred while trying to update the ACL policy."}
        </Notice>
      ) : undefined}
      <Tabs className="mb-4" label="ACL Editor" defaultValue="edit">
        <TabsList>
          <TabsTab value="edit">
            <div className="flex items-center gap-2">
              <Pencil className="p-1" />
              <span>Edit file</span>
            </div>
          </TabsTab>
          <TabsTab value="diff">
            <div className="flex items-center gap-2">
              <Eye className="p-1" />
              <span>Preview changes</span>
            </div>
          </TabsTab>
          <TabsTab value="preview">
            <div className="flex items-center gap-2">
              <FlaskConical className="p-1" />
              <span>Preview rules</span>
            </div>
          </TabsTab>
        </TabsList>
        <TabsPanel value="edit">
          <Suspense fallback={<Fallback />}>
            <LazyEditor isDisabled={disabled} onChange={setCodePolicy} value={codePolicy} />
          </Suspense>
        </TabsPanel>
        <TabsPanel value="diff">
          <Suspense fallback={<Fallback />}>
            <LazyDiffer left={policy} right={codePolicy} />
          </Suspense>
        </TabsPanel>
        <TabsPanel value="preview">
          <div className="flex flex-col items-center py-8">
            <Construction />
            <p className="mt-4 w-1/2 text-center">
              Previewing rules is not available yet. This feature is still in development and is
              pretty complicated to implement. Hopefully I will be able to get to it soon.
            </p>
          </div>
        </TabsPanel>
      </Tabs>
      <Button
        className="mr-2"
        disabled={
          disabled || fetcher.state !== "idle" || codePolicy.length === 0 || codePolicy === policy
        }
        onClick={() => {
          const formData = new FormData();
          formData.append("policy", codePolicy);
          fetcher.submit(formData, { method: "PATCH" });
        }}
        variant="heavy"
      >
        Save
      </Button>
      <Button
        disabled={disabled || fetcher.state !== "idle" || codePolicy === policy}
        onClick={() => {
          // Reset the editor to the original policy
          setCodePolicy(policy);
        }}
      >
        Discard Changes
      </Button>
    </div>
  );
}

// Local copy of the `isApiError` guard from
// ~/server/headscale/api/error-client — this module must not import
// server-only code.
function isHeadscaleApiError(error: unknown): error is {
  requestUrl: string;
  statusCode: number;
  rawData: string;
  data: Record<string, unknown> | null;
} {
  return (
    error != null &&
    typeof error === "object" &&
    "requestUrl" in error &&
    "statusCode" in error &&
    "rawData" in error &&
    "data" in error
  );
}

export function ErrorBoundary({ error }: Route.ErrorBoundaryProps) {
  if (
    isRouteErrorResponse(error) &&
    isHeadscaleApiError(error.data) &&
    error.data.rawData.includes("reading policy from path") &&
    error.data.rawData.includes("no such file or directory")
  ) {
    return (
      <div className="flex flex-col gap-4">
        <Card className="max-w-2xl" variant="flat">
          <div className="flex items-center justify-between gap-4">
            <Card.Title>ACL Policy Unavailable</Card.Title>
            <AlertCircle className="mb-2 h-6 w-6 text-red-500" />
          </div>
          <Card.Text>
            The ACL policy is currently unavailable because the policy file does not exist on the
            server. This usually indicates that Headscale is running in <Code>file</Code> mode for
            ACLs, and the specified policy file is missing.
          </Card.Text>
        </Card>
        <Card className="max-w-2xl" variant="flat">
          <Card.Text>
            In order to resolve this issue, there are two possible actions you can take:
          </Card.Text>
          <ul className="mt-2 ml-4 list-outside list-disc space-y-1 text-sm">
            <li>
              Create the ACL policy file at the specified path in your Headscale configuration.
            </li>
            <li>
              Alternatively, you can switch Headscale to use <Code>database</Code> mode for ACLs by
              updating your Headscale configuration. This will allow Headplane to manage the ACL
              policy directly through the web interface.
            </li>
          </ul>
        </Card>
      </div>
    );
  }

  return <PageError error={error} page="Access Control" />;
}
