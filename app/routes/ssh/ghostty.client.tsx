import { useEffect, useRef } from "react";
import { Restty } from "restty";
import type { GhosttyTheme } from "restty";
import type { PtyTransport } from "restty/internal";

import type { HeadplaneSSH, TunnelSession } from "./wasm.client";

const FONT_BASE = `${__PREFIX__}/fonts`;

// Ghostty's default canvas background is rgb(20,23,26) — a dark gray, not black.
// Override it so the terminal matches the page and pane container backgrounds.
const HEADPLANE_THEME: GhosttyTheme = {
  colors: {
    background: { r: 0, g: 0, b: 0 },
    foreground: { r: 235, g: 237, b: 242 },
    palette: [],
  },
  raw: {},
};

// Strips ANSI escape sequences so terminal output can be shown as plain text.
function stripAnsi(text: string) {
  // oxlint-disable-next-line no-control-regex
  return text.replace(/\u001b\[[0-9;?]*[ -/]*[@-~]/g, "").trim();
}

function createSSHTransport(
  ssh: HeadplaneSSH,
  ipAddress: string,
  username: string,
  password: string | undefined,
  onFailed: (output: string) => void,
): PtyTransport {
  let session: TunnelSession | null = null;
  let established = false;
  // Output received before the session is established (e.g. "SSH error: …").
  // The connecting overlay hides the terminal, so surface it as an error instead.
  let early = "";

  return {
    connect(options) {
      session = ssh.openTunnel({
        ipAddress,
        username,
        password,
        onData: (data) => {
          if (!established && early.length < 4096) early += data;
          options.callbacks.onData?.(data);
        },
        onConnect: () => {
          established = true;
          options.callbacks.onConnect?.();
        },
        onDisconnect: () => {
          options.callbacks.onDisconnect?.();
          session = null;
          if (!established) onFailed(stripAnsi(early));
        },
      });

      if (options.cols && options.rows) {
        session.resize(options.cols, options.rows);
      }
    },
    disconnect() {
      session?.close();
      session = null;
    },
    sendInput(data) {
      session?.writeInput(data);
      return session != null;
    },
    resize(cols, rows) {
      session?.resize(cols, rows);
      return session != null;
    },
    isConnected() {
      return session != null;
    },
    destroy() {
      session?.close();
      session = null;
    },
  };
}

interface GhosttyProps {
  ssh: HeadplaneSSH;
  ipAddress: string;
  username: string;
  password?: string;
  onConnected: () => void;
  onFailed: (output: string) => void;
}

export default function Ghostty({
  ssh,
  ipAddress,
  username,
  password,
  onConnected,
  onFailed,
}: GhosttyProps) {
  const divRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!divRef.current) return;

    const transport = createSSHTransport(ssh, ipAddress, username, password, onFailed);
    const restty = new Restty({
      root: divRef.current,
      createInitialPane: true,
      defaultContextMenu: false,
      shortcuts: false,
      searchUi: false,
      paneStyles: {
        inactivePaneOpacity: 1,
        activePaneOpacity: 1,
      },
      appOptions: {
        fontSize: 20,
        ligatures: true,
        fontPreset: "none",
        fontSources: [
          {
            type: "url",
            url: `${FONT_BASE}/JetBrainsMonoNLNerdFontMono-Regular.ttf`,
            label: "JetBrains Mono Nerd Font",
          },
          {
            type: "url",
            url: `${FONT_BASE}/JetBrainsMonoNLNerdFontMono-Bold.ttf`,
            label: "JetBrains Mono Nerd Font Bold",
          },
          {
            type: "url",
            url: `${FONT_BASE}/JetBrainsMonoNLNerdFontMono-Italic.ttf`,
            label: "JetBrains Mono Nerd Font Italic",
          },
          {
            type: "url",
            url: `${FONT_BASE}/JetBrainsMonoNLNerdFontMono-BoldItalic.ttf`,
            label: "JetBrains Mono Nerd Font Bold Italic",
          },
          {
            type: "url",
            url: `${FONT_BASE}/SymbolsNerdFontMono-Regular.ttf`,
            label: "Symbols Nerd Font",
          },
        ],
        ptyTransport: transport,
        callbacks: {
          onPtyStatus: (status) => {
            if (status === "connected") onConnected();
          },
        },
      },
    });

    restty.applyTheme(HEADPLANE_THEME);
    restty.updateSize(true);
    restty.connectPty();

    return () => {
      restty.destroy();
    };
  }, [ssh, ipAddress, username, password]);

  return <div className="min-h-0 min-w-0 flex-1 overflow-hidden bg-black" ref={divRef} />;
}
