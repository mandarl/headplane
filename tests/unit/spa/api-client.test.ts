// Phase 0 (SPA conversion): unit tests for the v1 JSON API client helpers
// (app/lib/api.ts). These encode the client/server contract: 401 → login
// redirect, {redirect} bodies → client-side navigation, error JSON bodies →
// returned for useActionData display.
import { beforeEach, describe, expect, test, vi } from "vitest";

import { apiAction, apiFetch, apiGet } from "~/lib/api";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function stubFetch(res: Response) {
  globalThis.fetch = vi.fn().mockResolvedValue(res) as unknown as typeof fetch;
}

async function catchRedirect(promise: Promise<unknown>): Promise<Response> {
  try {
    await promise;
  } catch (err) {
    if (err instanceof Response) return err;
    throw err;
  }
  throw new Error("expected a redirect to be thrown");
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe("apiFetch", () => {
  test("prefixes the v1 base path and includes credentials", async () => {
    stubFetch(jsonResponse(200, {}));
    await apiFetch("/boot");
    expect(globalThis.fetch).toHaveBeenCalledWith(
      `${__PREFIX__}/api/v1/boot`,
      expect.objectContaining({ credentials: "include" }),
    );
  });
});

describe("apiGet", () => {
  test("returns parsed JSON on 200", async () => {
    stubFetch(jsonResponse(200, { hello: "world" }));
    await expect(apiGet<{ hello: string }>("/boot")).resolves.toEqual({ hello: "world" });
  });

  test("401 throws a redirect to /login", async () => {
    stubFetch(jsonResponse(401, { error: "Unauthenticated" }));
    const res = await catchRedirect(apiGet("/boot"));
    expect(res.status).toBe(302);
    expect(res.headers.get("Location")).toBe("/login");
  });

  test("other non-2xx throws an Error", async () => {
    stubFetch(jsonResponse(500, { error: "boom" }));
    await expect(apiGet("/boot")).rejects.toThrow("GET /boot failed with status 500");
  });
});

describe("apiAction", () => {
  function postForm(): FormData {
    const form = new FormData();
    form.set("action_id", "rename");
    form.set("name", "new-name");
    return form;
  }

  test("sends the form as a JSON body", async () => {
    stubFetch(jsonResponse(200, { message: "ok" }));
    await apiAction("/machines/actions", postForm());
    const [, init] = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls[0] as [
      string,
      RequestInit,
    ];
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({ "Content-Type": "application/json" });
    expect(JSON.parse(init.body as string)).toEqual({ action_id: "rename", name: "new-name" });
  });

  test("{redirect} body throws a client-side redirect", async () => {
    stubFetch(jsonResponse(200, { redirect: "/machines" }));
    const res = await catchRedirect(apiAction("/machines/actions", postForm()));
    expect(res.status).toBe(302);
    expect(res.headers.get("Location")).toBe("/machines");
  });

  test("non-2xx with a JSON body returns the body (action-data semantics)", async () => {
    stubFetch(jsonResponse(400, { success: false, error: "bad tags" }));
    const body = await apiAction<{ success: boolean; error: string }>(
      "/machines/actions",
      postForm(),
    );
    expect(body).toEqual({ success: false, error: "bad tags" });
  });

  test("non-2xx without a body throws an Error", async () => {
    stubFetch(new Response("oops", { status: 500 }));
    await expect(apiAction("/machines/actions", postForm())).rejects.toThrow(
      "POST /machines/actions failed with status 500",
    );
  });

  test("401 throws a redirect to /login", async () => {
    stubFetch(jsonResponse(401, { error: "Unauthenticated" }));
    const res = await catchRedirect(apiAction("/machines/actions", postForm()));
    expect(res.status).toBe(302);
    expect(res.headers.get("Location")).toBe("/login");
  });
});
