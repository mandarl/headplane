import { Copy, Loader2 } from "lucide-react";
import { useEffect, useState } from "react";
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

const TIMEOUT_OPTIONS = [
  { label: "1 hour", mins: 60 },
  { label: "2 hours", mins: 120 },
  { label: "3 hours", mins: 180 },
  { label: "8 hours", mins: 480 },
] as const;

function useCountdown(expiresAt: string | undefined): string | null {
  const [remaining, setRemaining] = useState<string | null>(null);

  useEffect(() => {
    if (!expiresAt) {
      setRemaining(null);
      return;
    }

    function tick() {
      const secs = Math.max(0, Math.floor((new Date(expiresAt!).getTime() - Date.now()) / 1000));
      if (secs === 0) {
        setRemaining("Expired");
        return;
      }
      const h = Math.floor(secs / 3600);
      const m = Math.floor((secs % 3600) / 60);
      const s = secs % 60;
      const parts =
        h > 0
          ? [h, String(m).padStart(2, "0"), String(s).padStart(2, "0")]
          : [m, String(s).padStart(2, "0")];
      setRemaining(parts.join(":"));
    }

    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [expiresAt]);

  return remaining;
}

export default function RDPGateway({ node, isOpen, setIsOpen }: RDPGatewayProps) {
  const fetcher = useFetcher<GatewayResult>();
  const [timeoutMins, setTimeoutMins] = useState(180);

  const targetIp = node.ipAddresses[0] ?? "";
  const isLoading = fetcher.state !== "idle";

  const result = fetcher.data;
  const isActive = result?.success === true && result.host != null;
  const hasError = result?.success === false;

  // Only non-null when isActive is true; JSX uses it inside the isActive guard.
  const endpoint: string = isActive ? `${result!.host}:${result!.port}` : "";
  const countdown = useCountdown(isActive ? result?.expires_at : undefined);

  function enable() {
    const form = new FormData();
    form.set("action_id", "enable");
    form.set("target_ip", targetIp);
    form.set("hostname", node.givenName);
    form.set("timeout_mins", String(timeoutMins));
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
            {countdown && (
              <p className="mt-2 text-xs text-mist-500">
                Auto-closes in <span className="font-medium tabular-nums">{countdown}</span>
              </p>
            )}
          </div>
        )}

        {!isActive && (
          <div className="mt-3 flex items-center gap-2">
            <label
              className="shrink-0 text-sm text-mist-600 dark:text-mist-300"
              htmlFor="rdp-timeout"
            >
              Duration
            </label>
            <select
              className="rounded-md border border-mist-200 bg-white px-2 py-1 text-sm dark:border-mist-700 dark:bg-mist-800"
              disabled={isLoading}
              id="rdp-timeout"
              onChange={(e) => setTimeoutMins(Number(e.target.value))}
              value={timeoutMins}
            >
              {TIMEOUT_OPTIONS.map((o) => (
                <option key={o.mins} value={o.mins}>
                  {o.label}
                </option>
              ))}
            </select>
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
