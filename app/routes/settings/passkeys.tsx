import { KeyRound, Pencil, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { redirect, useRevalidator } from "react-router";

import Button from "~/components/button";
import Code from "~/components/code";
import Dialog, { DialogPanel } from "~/components/dialog";
import Input from "~/components/input";
import Link from "~/components/link";
import Notice from "~/components/notice";
import TableList from "~/components/table-list";
import Text from "~/components/text";
import Title from "~/components/title";
import { apiFetch, apiGet } from "~/lib/api";
import { isWebAuthnSupported, PasskeyCancelledError, registerPasskey } from "~/lib/passkey";
import { formatTimeDelta } from "~/utils/time";

import type { Route } from "./+types/passkeys";

export interface PasskeyCredential {
  id: string;
  label: string;
  transports: string[];
  backup_eligible: boolean;
  backup_state: boolean;
  created_at: number;
  last_used_at?: number | null;
}

type PasskeysData = {
  credentials: PasskeyCredential[];
  disabled: boolean;
};

export async function clientLoader(): Promise<PasskeysData> {
  try {
    const data = await apiGet<{ credentials: PasskeyCredential[] }>("/passkeys");
    return { credentials: data.credentials, disabled: false };
  } catch (error) {
    // Passkeys disabled server-side: the endpoint 404s. Show an
    // explanatory notice instead of the error boundary (the Settings
    // overview hides the link in this case, so this is only reachable
    // by direct URL).
    if (error instanceof Error && error.message.includes("status 404")) {
      return { credentials: [], disabled: true };
    }
    throw error;
  }
}

function formatDate(unixSeconds: number): string {
  return new Date(unixSeconds * 1000).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

// 401 from a mutation means the session expired mid-page: send the user
// back to login like apiGet/apiAction do, instead of blaming the server.
function throwIfUnauthorized(res: Response): void {
  if (res.status === 401) {
    throw redirect("/login");
  }
}

const transportNames: Record<string, string> = {
  hybrid: "Phone QR",
  internal: "Built-in",
  usb: "USB key",
  nfc: "NFC",
  ble: "Bluetooth",
};

function humanizeTransports(transports: string[]): string {
  return transports.map((t) => transportNames[t] ?? t).join(", ");
}

function SyncBadge({ cred }: { cred: PasskeyCredential }) {
  if (cred.backup_eligible && cred.backup_state) {
    return (
      <span className="rounded-full bg-green-100 px-2 py-0.5 text-xs font-medium text-green-800 dark:bg-green-900/40 dark:text-green-300">
        Synced passkey
      </span>
    );
  }
  return (
    <span className="rounded-full bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-800 dark:bg-amber-900/40 dark:text-amber-300">
      {cred.backup_eligible ? "Not synced" : "This device only"}
    </span>
  );
}

export default function Page({ loaderData: { credentials, disabled } }: Route.ComponentProps) {
  const revalidator = useRevalidator();
  const [addOpen, setAddOpen] = useState(false);
  const [label, setLabel] = useState("");
  const [busy, setBusy] = useState(false);
  const [dialogError, setDialogError] = useState<string | null>(null);
  const [renameTarget, setRenameTarget] = useState<PasskeyCredential | null>(null);
  const [renameLabel, setRenameLabel] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<PasskeyCredential | null>(null);

  const refresh = () => revalidator.revalidate();
  const labelValid = label.trim().length > 0;
  const renameValid = renameLabel.trim().length > 0;

  async function handleAdd() {
    if (!labelValid) {
      return;
    }
    setBusy(true);
    setDialogError(null);
    try {
      await registerPasskey(label.trim());
      setAddOpen(false);
      setLabel("");
      refresh();
    } catch (err) {
      // A cancelled ceremony needs no message; the dialog stays open so
      // the user can retry with a different authenticator.
      if (!(err instanceof PasskeyCancelledError)) {
        setDialogError(err instanceof Error ? err.message : "Could not register the passkey.");
      }
    } finally {
      setBusy(false);
    }
  }

  async function handleRename() {
    if (!renameTarget || !renameValid) {
      return;
    }
    setBusy(true);
    setDialogError(null);
    try {
      const res = await apiFetch("/passkeys", {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id: renameTarget.id, label: renameLabel.trim() }),
      });
      throwIfUnauthorized(res);
      if (!res.ok) {
        throw new Error("The server rejected the rename.");
      }
      setRenameTarget(null);
      refresh();
    } catch (err) {
      if (err instanceof Response) {
        throw err;
      }
      setDialogError(err instanceof Error ? err.message : "Could not rename the passkey.");
    } finally {
      setBusy(false);
    }
  }

  async function handleDelete() {
    if (!deleteTarget) {
      return;
    }
    setBusy(true);
    setDialogError(null);
    try {
      const res = await apiFetch("/passkeys", {
        method: "DELETE",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id: deleteTarget.id }),
      });
      throwIfUnauthorized(res);
      if (!res.ok) {
        throw new Error("The server rejected the deletion.");
      }
      setDeleteTarget(null);
      refresh();
    } catch (err) {
      if (err instanceof Response) {
        throw err;
      }
      setDialogError(err instanceof Error ? err.message : "Could not delete the passkey.");
    } finally {
      setBusy(false);
    }
  }

  // Don't let Escape/backdrop close a dialog mid-ceremony; the pending
  // WebAuthn promise would resolve into a closed dialog's stale state.
  const keepOpenWhileBusy = (open: boolean, close: () => void) => {
    if (!open && !busy) {
      close();
    }
  };

  return (
    <div className="flex max-w-(--breakpoint-lg) flex-col gap-8">
      <div className="flex w-full flex-col sm:w-2/3">
        <p className="text-md mb-8">
          <Link className="font-medium" to="/settings">
            Settings
          </Link>
          <span className="mx-2">/</span> Passkeys
        </p>
        <Title>Passkeys</Title>
        <Text>
          Passkeys let you sign in without your API key. They live in your authenticator — a synced
          provider like LastPass works on every computer you use, and on a computer without your
          provider you can still sign in by scanning the QR code with your phone.
        </Text>
      </div>

      {disabled ? (
        <Notice title="Passkeys are disabled" variant="warning">
          Passkey login is turned off on this server (<Code>webauthn.enabled: false</Code>). Remove
          that setting to register passkeys.
        </Notice>
      ) : (
        <>
          <div>
            <Button
              disabled={!isWebAuthnSupported()}
              onClick={() => {
                setDialogError(null);
                setLabel("");
                setAddOpen(true);
              }}
              variant="heavy"
            >
              <Plus className="h-4 w-4" /> Add passkey
            </Button>
            {!isWebAuthnSupported() ? (
              <Text className="mt-2 text-sm">
                This browser does not support WebAuthn, so new passkeys cannot be registered here.
              </Text>
            ) : undefined}
          </div>

          {credentials.length === 0 ? (
            <Text>
              No passkeys registered yet. Add one to sign in without your API key — a synced
              provider like LastPass works on all your computers.
            </Text>
          ) : (
            <TableList>
              {credentials.map((cred) => (
                <TableList.Item key={cred.id}>
                  <div className="flex items-center gap-3">
                    <KeyRound className="h-5 w-5 shrink-0 text-mist-500" />
                    <div>
                      <div className="flex items-center gap-2 font-medium">
                        {cred.label || "Unnamed passkey"}
                        <SyncBadge cred={cred} />
                      </div>
                      <div className="text-sm text-mist-500">
                        Added {formatDate(cred.created_at)}
                        {cred.last_used_at
                          ? ` · last used ${formatTimeDelta(new Date(cred.last_used_at * 1000))}`
                          : " · never used"}
                        {cred.transports.length > 0
                          ? ` · ${humanizeTransports(cred.transports)}`
                          : ""}
                      </div>
                    </div>
                  </div>
                  <div className="flex items-center gap-2">
                    <Button
                      aria-label={`Rename passkey ${cred.label || "Unnamed passkey"}`}
                      onClick={() => {
                        setDialogError(null);
                        setRenameLabel(cred.label);
                        setRenameTarget(cred);
                      }}
                      variant="ghost"
                    >
                      <Pencil className="h-4 w-4" />
                    </Button>
                    <Button
                      aria-label={`Delete passkey ${cred.label || "Unnamed passkey"}`}
                      onClick={() => {
                        setDialogError(null);
                        setDeleteTarget(cred);
                      }}
                      variant="ghost"
                    >
                      <Trash2 className="h-4 w-4 text-red-600 dark:text-red-400" />
                    </Button>
                  </div>
                </TableList.Item>
              ))}
            </TableList>
          )}

          <Dialog
            isOpen={addOpen}
            onOpenChange={(open) => keepOpenWhileBusy(open, () => setAddOpen(false))}
          >
            <DialogPanel
              hideFooter
              onSubmit={(e) => {
                e.preventDefault();
                void handleAdd();
              }}
            >
              <Title>Add passkey</Title>
              <Text>
                Give this passkey a name so you can recognize it later, then follow your browser's
                prompt. Use your synced provider (for example LastPass) so the passkey is available
                on all your computers.
              </Text>
              {dialogError ? (
                <Notice title="Couldn't register the passkey" variant="error">
                  {dialogError}
                </Notice>
              ) : undefined}
              <Input
                className="mt-4"
                label="Label"
                onChange={(value) => setLabel(value)}
                placeholder="e.g. LastPass"
                value={label}
              />
              <div className="mt-4 flex justify-end gap-2">
                <Button disabled={busy} onClick={() => setAddOpen(false)} variant="light">
                  Cancel
                </Button>
                <Button
                  disabled={busy || !labelValid}
                  onClick={() => void handleAdd()}
                  variant="heavy"
                >
                  {busy ? "Waiting…" : "Register passkey"}
                </Button>
              </div>
            </DialogPanel>
          </Dialog>

          <Dialog
            isOpen={renameTarget !== null}
            onOpenChange={(open) => keepOpenWhileBusy(open, () => setRenameTarget(null))}
          >
            <DialogPanel
              hideFooter
              onSubmit={(e) => {
                e.preventDefault();
                void handleRename();
              }}
            >
              <Title>Rename passkey</Title>
              {dialogError ? (
                <Notice title="Couldn't rename the passkey" variant="error">
                  {dialogError}
                </Notice>
              ) : undefined}
              <Input
                className="mt-4"
                label="Label"
                onChange={(value) => setRenameLabel(value)}
                value={renameLabel}
              />
              <div className="mt-4 flex justify-end gap-2">
                <Button disabled={busy} onClick={() => setRenameTarget(null)} variant="light">
                  Cancel
                </Button>
                <Button
                  disabled={busy || !renameValid}
                  onClick={() => void handleRename()}
                  variant="heavy"
                >
                  {busy ? "Saving…" : "Save"}
                </Button>
              </div>
            </DialogPanel>
          </Dialog>

          <Dialog
            isOpen={deleteTarget !== null}
            onOpenChange={(open) => keepOpenWhileBusy(open, () => setDeleteTarget(null))}
          >
            <DialogPanel hideFooter onSubmit={(e) => e.preventDefault()}>
              <Title>Delete passkey</Title>
              <Text>
                Delete "{deleteTarget?.label || "Unnamed passkey"}"? You won't be able to sign in
                with this passkey anymore, but your API key still works.
                {credentials.length === 1
                  ? " This is your last passkey — afterward you'll sign in with your API key."
                  : ""}{" "}
                This can't be undone.
              </Text>
              {dialogError ? (
                <Notice title="Couldn't delete the passkey" variant="error">
                  {dialogError}
                </Notice>
              ) : undefined}
              <div className="mt-4 flex justify-end gap-2">
                <Button disabled={busy} onClick={() => setDeleteTarget(null)} variant="light">
                  Cancel
                </Button>
                <Button disabled={busy} onClick={() => void handleDelete()} variant="danger">
                  {busy ? "Deleting…" : "Delete"}
                </Button>
              </div>
            </DialogPanel>
          </Dialog>
        </>
      )}
    </div>
  );
}
