import type { Capabilities } from "../capabilities";
import type { Transport } from "../transport";

export interface AuthApi {
  /**
   * Approve a pending Headscale authentication request.
   * Used by the Headplane agent to auto-approve its own registration.
   */
  approve(authId: string): Promise<void>;
}

export function makeAuthApi(
  transport: Transport,
  _capabilities: Capabilities,
  apiKey: string,
): AuthApi {
  return {
    approve: async (authId) => {
      await transport.request({
        method: "POST",
        path: "v1/auth/approve",
        apiKey,
        body: { authId },
      });
    },
  };
}
