import type { HostInfo, Service } from "~/types";

export function getTSVersion(host: HostInfo) {
  const { IPNVersion } = host;
  if (!IPNVersion) {
    return "Unknown";
  }

  // IPNVersion is <Semver>-<something>-<something>
  return IPNVersion.split("-")[0];
}

export function getOSInfo(host: HostInfo) {
  const { OS, OSVersion } = host;
  // OS follows runtime.GOOS but uses iOS and macOS instead of darwin
  const formattedOS = formatOS(OS);

  // Trim in case OSVersion is empty
  return `${formattedOS} ${OSVersion ?? ""}`.trim();
}

// Fallback descriptions for well-known ports when the client didn't report
// a process name in `Service.Description` (this happens on some platforms).
const WELL_KNOWN_PORTS: Record<number, string> = {
  22: "SSH",
  80: "HTTP",
  443: "HTTPS",
  3000: "Dev server",
  3389: "RDP",
  5000: "Dev server",
  8000: "HTTP (alt)",
  8080: "HTTP (alt)",
  8443: "HTTPS (alt)",
  9090: "Metrics",
};

/**
 * Best-effort human readable description of a service. Falls back to a
 * well-known-port lookup and finally to a generic label, since
 * `Service.Description` is frequently empty depending on OS/permissions.
 */
export function getServiceDescription(service: Service) {
  if (service.Description) {
    return service.Description;
  }

  return WELL_KNOWN_PORTS[service.Port] ?? "Unknown service";
}

// Only TCP services are meaningfully linkable as http(s):// URLs. UDP and
// internal Tailscale services (e.g. "peerapi4") aren't browsable.
export function isLinkableService(service: Service) {
  return service.Proto.toLowerCase() === "tcp";
}

function formatOS(os?: string) {
  switch (os) {
    case "macOS":
    case "iOS":
      return os;
    case "windows":
      return "Windows";
    case "linux":
      return "Linux";
    case undefined:
      return "Unknown";
    default:
      return os;
  }
}
