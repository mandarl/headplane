import { redirect } from "react-router";

import { isDataWithApiError } from "~/server/headscale/api/error-client";
import log from "~/utils/log";

import type { Route } from "./+types/page";

// MARK: Phase 0 interim — JSON failure contract.
//
// The login page is now an SPA route whose clientAction forwards the form to
// this endpoint with fetch (a document POST, not an RR data request) and
// reads the failure body as JSON. The failure MUST use status 200: React
// Router converts action Responses with a 4xx/5xx status into error pages
// for document requests, while a 200 Response passes through untouched.
// Success is still a 302 to /machines with the session Set-Cookie, which
// fetch follows.
function loginFailure(message: string): Response {
  return Response.json({ success: false, message });
}

export async function loginAction({ request, context }: Route.LoaderArgs) {
  const formData = await request.formData();
  const apiKey = formData.has("api_key") ? String(formData.get("api_key")) : undefined;

  if (apiKey === undefined) {
    log.warn("auth", "Request made without API key");
    log.warn(
      "auth",
      "If this is unexpected, ensure your reverse proxy (if applicable) is configured correctly",
    );
    return loginFailure("Missing API key. Please enter your API key.");
  }

  if (apiKey.length === 0) {
    log.warn("auth", "Request made with empty API key");
    log.warn(
      "auth",
      "If this is unexpected, ensure your reverse proxy (if applicable) is configured correctly",
    );
    return loginFailure("API key cannot be empty. Please enter a valid API key.");
  }

  // Build a client with the candidate API key the user just submitted, so the
  // GET /api/v1/apikey call below validates the key against Headscale itself.
  const api = context.headscale.client(apiKey);
  try {
    const apiKeys = await api.apiKeys.list();

    // We don't need to check for 0 API keys because this request cannot
    // be authenticated correctly without an API key
    //
    // 0.28.0 pointlessly added asterisks to the prefixes of API keys, which is
    // the dumbest thing I've ever seen.
    const lookup = apiKeys.find((key) => apiKey.startsWith(key.prefix.replaceAll("*", "")));
    if (!lookup) {
      return loginFailure("API key was not found in the Headscale database");
    }

    if (lookup.expiration === null || lookup.expiration === undefined) {
      log.error("auth", "Got an API key without an expiration");
      return loginFailure(
        "API key is malformed (missing expiration). Please generate a new API key.",
      );
    }

    const expiry = new Date(lookup.expiration);
    if (expiry.getTime() < Date.now()) {
      return loginFailure("API key has expired");
    }

    return redirect("/machines", {
      headers: {
        "Set-Cookie": await context.auth.createApiKeySession(
          apiKey,
          `${lookup.prefix}...`,
          expiry.getTime() - Date.now(),
        ),
      },
    });
  } catch (error) {
    // Check if this is a React Router DataWithResponseInit wrapping a Headscale API error
    if (isDataWithApiError(error)) {
      const apiError = error.data;
      // TODO: What in gods name is wrong with the headscale API?
      if (
        apiError.statusCode === 401 ||
        apiError.statusCode === 403 ||
        (apiError.statusCode === 500 && apiError.rawData.trim() === "Unauthorized")
      ) {
        return loginFailure("API key is invalid (it may be incorrect or expired)");
      }
    }

    log.error("auth", "Error while validating API key: %s", error);
    log.debug("auth", "Error details: %o", error);
    return loginFailure("Error while validating API key (see logs for details)");
  }
}
