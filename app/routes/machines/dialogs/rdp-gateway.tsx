import { Copy, Loader2 } from "lucide-react";
import { useFetcher } from "react-router";

import Button from "~/components/button";
import Code from "~/components/code";
import Dialog, { DialogPanel } from "~/components/dialog";
import Text from "~/components/text";
import Title from "~/components/title";
import type { PopulatedNode } from "~/utils/node-info";
import toast from "~/utils/toast";

interface RDPGatewayProps {
  node: PopulatedNode;
  isOpen: boolean;
  setIsOpen: (isOpen: boolean) => void;
}

interface GatewayResult {
  success: boolean;
  // Present when action_id="enable" succeeds:
  host?: string;
  port?: number;
  expires_at?: string;
  // Present on failure:
  error?: string;
}

export default function RDPGateway({ node, isOpen, setIsOpen }: RDPGatewayProps) {
  const fetcher = useFetcher<GatewayResult>();

  const targetIp = node.ipAddresses[0] ?? "";
  const isLoading = fetcher.state !== "idle";

  const result = fetcher.data;
  const isActive = result?.success === true && result.host != null;
  const hasError = result?.success === false;

  // Only non-null when isActive is true; JSX uses it inside the isActive guard.
  const endpoint: string = isActive ? `${result!.host}:${result!.port}` : "";

  function enable() {
    const form = new FormData();
    form.set("action_id", "enable");
    form.set("target_ip", targetIp);
    form.set("hostname", node.givenName);
    fetcher.submit(form, { method: "POST", action: "/api/rdp-gateway" });
  }

  function disable() {
    const form = new FormData();
    form.set("action_id", "disable");
    form.set("target_ip", targetIp);
    form.set("hostname", node.givenName);
    fetcher.submit(form, { method: "POST", action: "/api/rdp-gateway" });
  }

  async function copyEndpoint() {
    await navigator.clipboard.writeText(endpoint);
    toast("Copied to clipboard");
  }

  return (
    <Dialog isOpen={isOpen} onOpenChange={setIsOpen}>
      <DialogPanel variant="unactionable">
        <Title>RDP Gateway — {node.givenName}</Title>

        {!isActive && (
          <Text className="mb-2">
            Open a temporary public RDP port for <Code>{node.givenName}</Code>. The port will be
            scoped to your current IP address. Connect with a native RDP client using the address
            shown after enabling.
          </Text>
        )}

        {hasError && (
          <p className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-600 dark:bg-red-950/40 dark:text-red-400">
            {result?.error ?? "An unexpected error occurred."}
          </p>
        )}

        {isActive && (
          <div className="rounded-md border border-mist-200 bg-mist-50 p-3 dark:border-mist-700 dark:bg-mist-800/50">
            <p className="mb-1 text-xs font-medium tracking-wide text-mist-500 uppercase">
              Connect using
            </p>
            <div className="flex items-center gap-2">
              <Code className="text-base">{endpoint}</Code>
              <button
                aria-label="Copy RDP address"
                className="rounded p-1 text-mist-400 hover:bg-mist-200 hover:text-mist-700 dark:hover:bg-mist-700 dark:hover:text-mist-200"
                onClick={copyEndpoint}
                type="button"
              >
                <Copy className="h-4 w-4" />
              </button>
            </div>
            {result?.expires_at && (
              <p className="mt-2 text-xs text-mist-500">
                Auto-closes at{" "}
                <span className="font-medium">
                  {new Date(result.expires_at).toLocaleTimeString()}
                </span>
              </p>
            )}
          </div>
        )}

        <div className="mt-4 flex gap-2">
          {isLoading ? (
            <Button disabled variant="heavy">
              <Loader2 className="h-4 w-4 animate-spin" />
              {isActive ? "Disabling…" : "Enabling…"}
            </Button>
          ) : isActive ? (
            <Button onClick={disable} variant="danger">
              Disable Now
            </Button>
          ) : (
            <Button onClick={enable} variant="heavy">
              Enable Gateway
            </Button>
          )}
        </div>
      </DialogPanel>
    </Dialog>
  );
}
