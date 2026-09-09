// MARK: v1 JSON API client
//
// Client-side fetch helpers for the `/admin/api/v1/*` JSON API served by the
// Go server (and by the interim Node stub during Phase 0). All requests are
// same-origin with `credentials: "include"` so the session cookie rides along.
//
// Conventions (see docs/development/go-server-spa-rfc.md and the routes
// contract in the Phase 0 commit):
// - Unauthenticated → HTTP 401. Client loaders translate that into a
//   redirect to `/login`, mirroring the old server loaders.
// - Actions that used to `return redirect(...)` now return
//   `{ "redirect": "<path>" }` with HTTP 200; the helpers below re-throw those
//   as client-side redirects so `<Form>`/fetchers keep navigating.
import { redirect } from "react-router";

function apiUrl(path: string): string {
  return `${__PREFIX__}/api/v1${path}`;
}

function formToJson(form: FormData): Record<string, string | string[]> {
  const out: Record<string, string | string[]> = {};
  for (const [key, value] of form.entries()) {
    if (typeof value !== "string") continue;
    const prev = out[key];
    if (prev === undefined) {
      out[key] = value;
    } else if (Array.isArray(prev)) {
      prev.push(value);
    } else {
      out[key] = [prev, value];
    }
  }
  return out;
}

/** Raw fetch against the v1 JSON API (same origin, session cookie included). */
export async function apiFetch(path: string, init?: RequestInit): Promise<Response> {
  return fetch(apiUrl(path), { credentials: "include", ...init });
}

/**
 * GET and parse JSON.
 * - 401 → throw redirect("/login") (was: server loader redirect + session destroy)
 * - other non-2xx → throw Error (lands in the route ErrorBoundary,
 *   mirroring a throwing server loader)
 */
export async function apiGet<T>(path: string): Promise<T> {
  const res = await apiFetch(path);
  if (res.status === 401) {
    throw redirect("/login");
  }
  if (!res.ok) {
    throw new Error(`GET ${path} failed with status ${res.status}`);
  }
  return (await res.json()) as T;
}

/**
 * POST/PATCH/DELETE a form as a JSON body to a v1 action endpoint.
 * - 401 → throw redirect("/login")
 * - `{ "redirect": path }` body → throw redirect(path)
 * - non-2xx WITH a JSON body → return the body (mirrors React Router action
 *   `data(payload, { status })` semantics → surfaces via `useActionData`)
 * - non-2xx WITHOUT a body → throw Error (→ error boundary)
 */
export async function apiAction<T>(path: string, form: FormData, method = "POST"): Promise<T> {
  const res = await apiFetch(path, {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(formToJson(form)),
  });
  if (res.status === 401) {
    throw redirect("/login");
  }
  const data = (await res.json().catch(() => null)) as T | null;
  if (
    data &&
    typeof data === "object" &&
    "redirect" in data &&
    typeof (data as { redirect: unknown }).redirect === "string"
  ) {
    throw redirect((data as { redirect: string }).redirect);
  }
  if (!res.ok) {
    if (data !== null) {
      return data;
    }
    throw new Error(`${method} ${path} failed with status ${res.status}`);
  }
  return data as T;
}
