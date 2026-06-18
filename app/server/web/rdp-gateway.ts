// RdpGatewayClient is a thin HTTP client that talks to a user-configured
// webhook. The webhook is responsible for the actual relay setup (e.g.
// starting a socat forwarder and toggling a cloud firewall rule). Headplane
// only knows about the generic enable/disable contract below.
//
// Webhook contract
// ─────────────────
// POST {webhook_url}
// Authorization: Bearer {webhook_token}   (only if configured)
// Content-Type: application/json
//
// Enable:
//   { "action": "enable", "target_ip": "100.x.y.z",
//     "hostname": "vdi-name", "caller_ip": "1.2.3.4" }
//   → 200 { "host": "...", "port": 33001, "expires_at": "<ISO8601>" }
//
// Disable:
//   { "action": "disable", "target_ip": "100.x.y.z", "hostname": "vdi-name" }
//   → 200 { "ok": true }

export interface RdpGatewayEnableResult {
  /** Publicly reachable hostname or IP of the relay host. */
  host: string;
  /** TCP port that has been opened on the relay host. */
  port: number;
  /** ISO8601 timestamp when the gateway will auto-close this session. */
  expires_at: string;
}

interface RdpGatewayConfig {
  webhook_url: string;
  webhook_token?: string | undefined;
}

export class RdpGatewayClient {
  private config: RdpGatewayConfig;

  constructor(config: RdpGatewayConfig) {
    this.config = config;
  }

  private get headers(): HeadersInit {
    const h: Record<string, string> = { "Content-Type": "application/json" };
    if (this.config.webhook_token) {
      h["Authorization"] = `Bearer ${this.config.webhook_token}`;
    }
    return h;
  }

  async enable(
    targetIp: string,
    hostname: string,
    callerIp: string,
  ): Promise<RdpGatewayEnableResult> {
    const res = await fetch(this.config.webhook_url, {
      method: "POST",
      headers: this.headers,
      body: JSON.stringify({
        action: "enable",
        target_ip: targetIp,
        hostname,
        caller_ip: callerIp,
      }),
    });

    if (!res.ok) {
      const text = await res.text().catch(() => "");
      throw new Error(`RDP gateway webhook error: ${res.status} ${text}`.trim());
    }

    return res.json() as Promise<RdpGatewayEnableResult>;
  }

  async disable(targetIp: string, hostname: string): Promise<void> {
    const res = await fetch(this.config.webhook_url, {
      method: "POST",
      headers: this.headers,
      body: JSON.stringify({ action: "disable", target_ip: targetIp, hostname }),
    });

    if (!res.ok) {
      const text = await res.text().catch(() => "");
      throw new Error(`RDP gateway webhook error: ${res.status} ${text}`.trim());
    }
  }
}
