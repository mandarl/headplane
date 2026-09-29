// MARK: WebAuthn (passkey) client ceremonies
//
// Helpers for the passkey registration and login ceremonies. The server
// speaks raw WebAuthn JSON (navigator.credentials.create/get wire format);
// these helpers translate between that JSON and the browser API, which
// needs ArrayBuffers instead of base64url strings.
//
// Ceremony flow (both registration and login):
//  1. POST the .../options endpoint → { token, options }
//  2. navigator.credentials.create/get({ publicKey: decode(options) })
//  3. POST .../verify?token=… with encode(credential) as the JSON body

function apiUrl(path: string): string {
  return `${__PREFIX__}/api/v1${path}`;
}

function b64ToBytes(b64url: string): ArrayBuffer {
  const b64 = b64url.replace(/-/g, "+").replace(/_/g, "/");
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) {
    bytes[i] = bin.charCodeAt(i);
  }
  return bytes.buffer as ArrayBuffer;
}

function bytesToB64(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf);
  let bin = "";
  for (let i = 0; i < bytes.length; i++) {
    bin += String.fromCharCode(bytes[i]);
  }
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// decodeOptions converts base64url fields in creation/request options to
// ArrayBuffers for the browser API. `id`/`rawId` are absent in options.
function decodeOptions<T>(options: T): T {
  const out = structuredClone(options) as Record<string, unknown>;
  if (typeof out["challenge"] === "string") {
    out["challenge"] = b64ToBytes(out["challenge"]);
  }
  const user = out["user"] as Record<string, unknown> | undefined;
  if (user && typeof user["id"] === "string") {
    user["id"] = b64ToBytes(user["id"]);
  }
  for (const key of ["excludeCredentials", "allowCredentials"]) {
    const list = out[key] as Array<Record<string, unknown>> | undefined;
    if (Array.isArray(list)) {
      for (const cred of list) {
        if (typeof cred["id"] === "string") {
          cred["id"] = b64ToBytes(cred["id"]);
        }
      }
    }
  }
  return out as T;
}

// encodeCredential converts the browser credential back to base64url JSON.
function encodeCredential(cred: Credential): Record<string, unknown> {
  const out: Record<string, unknown> = {
    id: (cred as PublicKeyCredential).id,
    rawId: bytesToB64((cred as PublicKeyCredential).rawId),
    type: cred.type,
  };
  const response = (cred as PublicKeyCredential).response;
  const respJson: Record<string, unknown> = {};
  for (const key of [
    "clientDataJSON",
    "attestationObject",
    "authenticatorData",
    "signature",
    "userHandle",
  ] as const) {
    const value = (response as unknown as Record<string, ArrayBuffer | null>)[key];
    if (value) {
      respJson[key] = bytesToB64(value);
    }
  }
  out["response"] = respJson;
  const ext = (cred as PublicKeyCredential).getClientExtensionResults();
  if (ext && Object.keys(ext).length > 0) {
    out["clientExtensionResults"] = ext;
  }
  return out;
}

/** True when this browser can do WebAuthn at all. */
export function isWebAuthnSupported(): boolean {
  return typeof window !== "undefined" && window.PublicKeyCredential !== undefined;
}

async function postOptions(path: string): Promise<{
  token: string;
  options: PublicKeyCredentialCreationOptions | PublicKeyCredentialRequestOptions;
}> {
  let res: Response;
  try {
    res = await fetch(apiUrl(path), { method: "POST", credentials: "include" });
  } catch {
    throw new Error("Couldn't reach the server. Check your connection and try again.");
  }
  if (!res.ok) {
    // Prefer the server's own error message when it sent one.
    const body = (await res.json().catch(() => null)) as {
      error?: unknown;
      message?: unknown;
    } | null;
    const serverMessage =
      typeof body?.error === "string"
        ? body.error
        : typeof body?.message === "string"
          ? body.message
          : null;
    throw new Error(serverMessage ?? `Passkey ceremony failed to start (HTTP ${res.status})`);
  }
  const data = (await res.json()) as { token: string; options: unknown };
  // go-webauthn wraps the ceremony in a {"publicKey": ...} envelope
  // (protocol.CredentialCreation / protocol.CredentialAssertion), but the
  // WebAuthn API and everything below want the bare options object.
  const raw = data.options as { publicKey?: unknown } | null | undefined;
  const unwrapped =
    raw != null &&
    typeof raw === "object" &&
    "publicKey" in raw &&
    raw.publicKey != null &&
    typeof raw.publicKey === "object"
      ? raw.publicKey
      : raw;
  return {
    token: data.token,
    options: unwrapped as
      | PublicKeyCredentialCreationOptions
      | PublicKeyCredentialRequestOptions,
  };
}

/**
 * True when the DOMException is a user-cancelled ceremony rather than a
 * timeout. Browsers report both as NotAllowedError, so the elapsed time is
 * compared against the ceremony timeout the server sent (with headroom):
 * a cancel that arrives after the timeout window must have been a timeout.
 */
function isUserCancel(error: unknown, startedAt: number, timeoutMs?: number): boolean {
  if (!(error instanceof DOMException) || error.name !== "NotAllowedError") {
    return false;
  }
  if (timeoutMs === undefined) {
    return true;
  }
  return Date.now() - startedAt < timeoutMs - 5000;
}

/**
 * One-line ceremony diagnostics for the browser console. Only fires during
 * user-initiated passkey ceremonies, so it stays out of the way otherwise.
 */
function log(step: string, detail?: unknown): void {
  if (detail === undefined) {
    // eslint-disable-next-line no-console
    console.log(`[passkey] ${step}`);
  } else {
    // eslint-disable-next-line no-console
    console.log(`[passkey] ${step}`, detail);
  }
}

/**
 * Run a WebAuthn ceremony call with a hard client-side deadline. A browser
 * can leave navigator.credentials.create/get pending forever without ever
 * showing its prompt (for example when an earlier ceremony got wedged);
 * without this bound the UI would sit on its busy state indefinitely. The
 * deadline tracks the server-advertised ceremony timeout.
 */
async function ceremonyWithTimeout<T>(
  options: { timeout?: number },
  run: (signal: AbortSignal) => Promise<T>,
): Promise<T> {
  const timeoutMs = typeof options.timeout === "number" ? options.timeout : 120000;
  const ctrl = new AbortController();
  const timer = setTimeout(() => {
    log(
      `client-side timeout fired after ${timeoutMs + 5000}ms; aborting browser call`,
    );
    ctrl.abort();
  }, timeoutMs + 5000);
  try {
    return await run(ctrl.signal);
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Run a passkey registration ceremony. `label` names the new passkey.
 * Throws on failure with a human-readable message; a user-cancelled
 * ceremony throws PasskeyCancelledError so callers can stay quiet.
 */
export async function registerPasskey(label: string): Promise<void> {
  log("register: requesting options");
  const { token, options } = await postOptions("/passkeys/register/options");
  log("register: options received", {
    rpId: options.rp?.id,
    timeout: options.timeout,
    residentKey: options.authenticatorSelection?.residentKey,
    userVerification: options.authenticatorSelection?.userVerification,
    hints: (options as { hints?: string[] }).hints,
  });
  try {
    const uvpaa =
      await PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable?.();
    log("register: isUserVerifyingPlatformAuthenticatorAvailable =", uvpaa);
  } catch (error) {
    log("register: platform-authenticator check threw", error);
  }
  const startedAt = Date.now();
  let credential: Credential | null;
  try {
    log("register: calling navigator.credentials.create()");
    credential = (await ceremonyWithTimeout(options, (signal) =>
      navigator.credentials.create({
        publicKey: {
          ...(decodeOptions(options) as PublicKeyCredentialCreationOptions),
          signal,
        },
      }),
    )) as Credential | null;
    log("register: create() resolved", { id: credential?.id?.slice(0, 16) });
  } catch (error) {
    log(
      "register: create() threw",
      error instanceof DOMException ? `${error.name}: ${error.message}` : error,
    );
    if (isUserCancel(error, startedAt, options.timeout)) {
      throw new PasskeyCancelledError();
    }
    throw new Error("The passkey request timed out. Try again.");
  }
  if (!credential) {
    throw new Error("The browser did not create a passkey.");
  }
  log("register: verifying with server");
  const res = await fetch(
    `${apiUrl("/passkeys/register/verify")}?token=${encodeURIComponent(token)}&label=${encodeURIComponent(label)}`,
    {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(encodeCredential(credential)),
    },
  );
  if (!res.ok) {
    throw new Error("The server rejected the new passkey.");
  }
  log("register: verify ok");
}

/** Distinguishes a deliberate ceremony cancel from other failures. */
export class PasskeyCancelledError extends Error {
  constructor() {
    super("cancelled");
    this.name = "PasskeyCancelledError";
  }
}

export type PasskeyLoginResult = "ok" | "cancelled" | "timeout" | "failed";

/**
 * Run a passkey login ceremony. The caller should navigate to /machines
 * on "ok"; "cancelled" is a deliberate dismiss and needs no message.
 */
export async function loginWithPasskey(): Promise<PasskeyLoginResult> {
  log("login: requesting options");
  const { token, options } = await postOptions("/passkeys/login/options");
  log("login: options received", {
    rpId: options.rpId,
    timeout: options.timeout,
    hints: (options as { hints?: string[] }).hints,
  });
  const startedAt = Date.now();
  let credential: Credential | null;
  try {
    log("login: calling navigator.credentials.get()");
    credential = (await ceremonyWithTimeout(options, (signal) =>
      navigator.credentials.get({
        publicKey: {
          ...(decodeOptions(options) as PublicKeyCredentialRequestOptions),
          signal,
        },
      }),
    )) as Credential | null;
    log("login: get() resolved", { id: credential?.id?.slice(0, 16) });
  } catch (error) {
    log(
      "login: get() threw",
      error instanceof DOMException ? `${error.name}: ${error.message}` : error,
    );
    return isUserCancel(error, startedAt, options.timeout) ? "cancelled" : "timeout";
  }
  if (!credential) {
    return "failed";
  }
  log("login: verifying with server");
  const res = await fetch(
    `${apiUrl("/passkeys/login/verify")}?token=${encodeURIComponent(token)}`,
    {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(encodeCredential(credential)),
    },
  );
  if (!res.ok) {
    return "failed";
  }
  const data = (await res.json().catch(() => null)) as { success?: boolean } | null;
  return data?.success === true ? "ok" : "failed";
}
