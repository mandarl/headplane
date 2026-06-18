import { join } from "node:path";

import log from "~/utils/log";

import type { HeadplaneConfig } from "./config/config-schema";
import { loadIntegration } from "./config/integration";
import { createDbClient } from "./db/client.server";
import { disabled, enabled, type Feature } from "./feature";
import { createHeadscale, type HeadscaleClient } from "./headscale/api";
import { loadHeadscaleConfig } from "./headscale/config-loader";
import { createLiveStore, nodesResource, usersResource } from "./headscale/live-store";
import { type AgentManager, createAgentManager } from "./hp-agent";
import { createOidcService, type OidcService } from "./oidc/provider";
import { createAuthService, type Principal } from "./web/auth";
import { RdpGatewayClient } from "./web/rdp-gateway";

export type AppContext = Awaited<ReturnType<typeof createAppContext>>;

declare module "react-router" {
  interface AppLoadContext extends AppContext {}
}

export async function createAppContext(config: HeadplaneConfig) {
  const db = await createDbClient(join(config.server.data_path, "hp_persist.db"));
  const headscale = await createHeadscale({
    url: config.headscale.url,
    certPath: config.headscale.tls_cert_path,
  });

  // Resolve the Headscale API key: headscale.api_key takes precedence,
  // falling back to the deprecated oidc.headscale_api_key for compatibility.
  const headscaleApiKey = config.headscale.api_key ?? config.oidc?.headscale_api_key;

  const agents = await buildAgents(
    config,
    headscale.capabilities.preAuthKeysHaveStableIds,
    headscaleApiKey ? headscale.client(headscaleApiKey) : undefined,
    db,
  );
  const rdpGateway = buildRdpGateway(config);

  const auth = createAuthService({
    secret: config.server.cookie_secret,
    headscaleApiKey,
    db,
    cookie: {
      name: "_hp_auth",
      secure: config.server.cookie_secure,
      maxAge: config.server.cookie_max_age,
      domain: config.server.cookie_domain,
    },
  });

  const oidc = buildOidc(config, headscaleApiKey);

  const hsLive = createLiveStore([nodesResource, usersResource]);
  const hs = await loadHeadscaleConfig(
    config.headscale.config_path,
    config.headscale.config_strict,
    config.headscale.dns_records_path,
  );
  const integration = await loadIntegration(config.integration);

  // Disposers run in reverse-registration order on shutdown.
  const disposers: Array<() => Promise<void> | void> = [
    () => auth.stop(),
    () => hsLive.dispose(),
    () => headscale.dispose(),
  ];
  if (agents.state === "enabled") {
    disposers.push(() => agents.value.dispose());
  }

  async function apiForRequest(
    request: Request,
  ): Promise<{ principal: Principal; api: HeadscaleClient }> {
    const principal = await auth.require(request);
    const apiKey = auth.getHeadscaleApiKey(principal);
    return { principal, api: headscale.client(apiKey) };
  }

  function startServices() {
    auth.start();
  }

  async function dispose() {
    for (const d of [...disposers].reverse()) {
      try {
        await d();
      } catch (error) {
        log.warn("server", "Error during shutdown: %s", String(error));
      }
    }
  }

  return {
    config,
    db,
    headscale,
    headscaleApiKey,
    agents,
    rdpGateway,
    auth,
    oidc,
    hsLive,
    hs,
    integration,
    apiForRequest,
    startServices,
    dispose,
  };
}

function buildOidc(
  config: HeadplaneConfig,
  headscaleApiKey: string | undefined,
): Feature<OidcService> {
  if (!config.oidc) {
    return disabled("OIDC is not configured");
  }
  if (config.oidc.enabled === false) {
    return disabled("OIDC is disabled in the configuration");
  }
  if (!headscaleApiKey) {
    return disabled("OIDC requires headscale.api_key to be configured");
  }

  return enabled(
    createOidcService({
      issuer: config.oidc.issuer,
      clientId: config.oidc.client_id,
      clientSecret: config.oidc.client_secret,
      baseUrl: config.server.base_url ?? "",
      authorizationEndpoint: config.oidc.authorization_endpoint,
      tokenEndpoint: config.oidc.token_endpoint,
      userinfoEndpoint: config.oidc.userinfo_endpoint,
      endSessionEndpoint: config.oidc.end_session_endpoint,
      tokenEndpointAuthMethod:
        config.oidc.token_endpoint_auth_method === "client_secret_jwt"
          ? undefined
          : config.oidc.token_endpoint_auth_method,
      usePkce: config.oidc.use_pkce,
      scope: config.oidc.scope,
      subjectClaims: config.oidc.subject_claims,
      allowWeakRsaKeys: config.oidc.allow_weak_rsa_keys,
      extraParams: config.oidc.extra_params,
      profilePictureSource: config.oidc.profile_picture_source,
      postLogoutRedirectUri: config.oidc.post_logout_redirect_uri,
    }),
  );
}

function buildRdpGateway(config: HeadplaneConfig): Feature<RdpGatewayClient> {
  const gw = config.rdp_gateway;
  if (!gw) {
    return disabled("RDP gateway is not configured");
  }
  if (gw.enabled === false) {
    return disabled("RDP gateway is disabled in the configuration");
  }
  return enabled(
    new RdpGatewayClient({ webhook_url: gw.webhook_url, webhook_token: gw.webhook_token }),
  );
}

async function buildAgents(
  config: HeadplaneConfig,
  supportsTagOnlyKeys: boolean,
  apiClient: HeadscaleClient | undefined,
  db: Awaited<ReturnType<typeof createDbClient>>,
): Promise<Feature<AgentManager>> {
  const agentConfig = config.integration?.agent;
  if (!agentConfig?.enabled) {
    return disabled("Agent is not enabled in the configuration");
  }
  if (!apiClient) {
    return disabled("Agent requires headscale.api_key to be configured");
  }
  if (!supportsTagOnlyKeys) {
    return disabled("Agent requires Headscale 0.28 or newer");
  }

  const manager = await createAgentManager(
    agentConfig,
    config.headscale.url,
    apiClient,
    supportsTagOnlyKeys,
    db,
  );
  if (!manager) {
    return disabled("Agent failed to initialize (see logs)");
  }
  return enabled(manager);
}
