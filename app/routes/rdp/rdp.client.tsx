import { useEffect, useRef, useState } from "react";
import { Loader2 } from "lucide-react";

// e.code → RDP PS/2 Set-1 scancode. Keys with bit 0x100 set are extended
// (KBDFLAGS_EXTENDED) and require that flag in the RDP input PDU.
const CODE_TO_SCANCODE: Record<string, number> = {
  Escape: 0x01, F1: 0x3B, F2: 0x3C, F3: 0x3D, F4: 0x3E,
  F5: 0x3F, F6: 0x40, F7: 0x41, F8: 0x42, F9: 0x43,
  F10: 0x44, F11: 0x57, F12: 0x58,
  PrintScreen: 0x137, ScrollLock: 0x46, Pause: 0x45,
  Backquote: 0x29,
  Digit1: 0x02, Digit2: 0x03, Digit3: 0x04, Digit4: 0x05,
  Digit5: 0x06, Digit6: 0x07, Digit7: 0x08, Digit8: 0x09,
  Digit9: 0x0A, Digit0: 0x0B,
  Minus: 0x0C, Equal: 0x0D, Backspace: 0x0E,
  Tab: 0x0F,
  KeyQ: 0x10, KeyW: 0x11, KeyE: 0x12, KeyR: 0x13, KeyT: 0x14,
  KeyY: 0x15, KeyU: 0x16, KeyI: 0x17, KeyO: 0x18, KeyP: 0x19,
  BracketLeft: 0x1A, BracketRight: 0x1B, Enter: 0x1C,
  CapsLock: 0x3A,
  KeyA: 0x1E, KeyS: 0x1F, KeyD: 0x20, KeyF: 0x21, KeyG: 0x22,
  KeyH: 0x23, KeyJ: 0x24, KeyK: 0x25, KeyL: 0x26,
  Semicolon: 0x27, Quote: 0x28, Backslash: 0x2B,
  ShiftLeft: 0x2A, IntlBackslash: 0x56,
  KeyZ: 0x2C, KeyX: 0x2D, KeyC: 0x2E, KeyV: 0x2F, KeyB: 0x30,
  KeyN: 0x31, KeyM: 0x32, Comma: 0x33, Period: 0x34, Slash: 0x35,
  ShiftRight: 0x36,
  ControlLeft: 0x1D, MetaLeft: 0x15B, AltLeft: 0x38,
  Space: 0x39,
  AltRight: 0x138, MetaRight: 0x15C, ContextMenu: 0x15D,
  ControlRight: 0x11D,
  Insert: 0x152, Home: 0x147, PageUp: 0x149,
  Delete: 0x153, End: 0x14F, PageDown: 0x151,
  ArrowUp: 0x148, ArrowLeft: 0x14B, ArrowDown: 0x150, ArrowRight: 0x14D,
  NumLock: 0x45,
  NumpadDivide: 0x135, NumpadMultiply: 0x37,
  NumpadSubtract: 0x4A, NumpadAdd: 0x4E, NumpadEnter: 0x11C,
  NumpadDecimal: 0x53,
  Numpad0: 0x52, Numpad1: 0x4F, Numpad2: 0x50, Numpad3: 0x51,
  Numpad4: 0x4B, Numpad5: 0x4C, Numpad6: 0x4D,
  Numpad7: 0x47, Numpad8: 0x48, Numpad9: 0x49,
};

import type { HeadplaneRDP, RDPSession } from "./wasm.client";
import { loadHeadplaneRDPWASM } from "./wasm.client";

interface RDPCanvasProps {
  rdp: HeadplaneRDP;
  ipAddress: string;
  username: string;
  password: string;
  domain?: string;
  onConnected: () => void;
  onError: (msg: string) => void;
}

export default function RDPCanvas({ rdp, ipAddress, username, password, domain, onConnected, onError }: RDPCanvasProps) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const sessionRef = useRef<RDPSession | null>(null);

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;

    const ctx = canvas.getContext("2d");
    if (!ctx) return;

    canvas.width = window.innerWidth;
    canvas.height = window.innerHeight;

    let closed = false;

    const session = rdp.openSession({
      ipAddress,
      username,
      password,
      domain: domain ?? "",
      width: canvas.width,
      height: canvas.height,
      onUpdate: (x, y, w, h, pixels) => {
        const imageData = new ImageData(pixels, w, h);
        ctx.putImageData(imageData, x, y);
      },
      onConnect: () => {
        onConnected();
      },
      onDisconnect: () => {
        if (!closed) onError("Session ended by remote host.");
      },
      onError: (msg) => {
        console.error("[rdp] session error:", msg);
        onError(msg);
      },
    });

    sessionRef.current = session;

    function onKeyDown(e: KeyboardEvent) {
      e.preventDefault();
      const sc = CODE_TO_SCANCODE[e.code];
      if (sc !== undefined) sessionRef.current?.sendKey(sc, true);
    }
    function onKeyUp(e: KeyboardEvent) {
      e.preventDefault();
      const sc = CODE_TO_SCANCODE[e.code];
      if (sc !== undefined) sessionRef.current?.sendKey(sc, false);
    }
    function onMouseMove(e: MouseEvent) {
      sessionRef.current?.sendMouse(-1, e.offsetX, e.offsetY, false);
    }
    function onMouseDown(e: MouseEvent) {
      sessionRef.current?.sendMouse(e.button, e.offsetX, e.offsetY, true);
    }
    function onMouseUp(e: MouseEvent) {
      sessionRef.current?.sendMouse(e.button, e.offsetX, e.offsetY, false);
    }
    function onContextMenu(e: MouseEvent) {
      e.preventDefault();
    }

    canvas.addEventListener("keydown", onKeyDown);
    canvas.addEventListener("keyup", onKeyUp);
    canvas.addEventListener("mousemove", onMouseMove);
    canvas.addEventListener("mousedown", onMouseDown);
    canvas.addEventListener("mouseup", onMouseUp);
    canvas.addEventListener("contextmenu", onContextMenu);
    canvas.focus();

    return () => {
      closed = true;
      canvas.removeEventListener("keydown", onKeyDown);
      canvas.removeEventListener("keyup", onKeyUp);
      canvas.removeEventListener("mousemove", onMouseMove);
      canvas.removeEventListener("mousedown", onMouseDown);
      canvas.removeEventListener("mouseup", onMouseUp);
      canvas.removeEventListener("contextmenu", onContextMenu);
      sessionRef.current?.close();
      sessionRef.current = null;
    };
  }, [rdp, ipAddress, username, password, domain]);

  return (
    <canvas
      ref={canvasRef}
      tabIndex={0}
      className="block h-screen w-screen outline-none"
      style={{ background: "#000" }}
    />
  );
}

interface RDPConsoleProps {
  hostname: string;
  username: string;
  password: string;
  domain?: string;
  node: {
    ipAddress: string;
    controlURL: string;
    preAuthKey: string;
    ephemeralHostname: string;
  };
}

export function RDPConsole({ hostname, username, password, domain, node }: RDPConsoleProps) {
  const [rdp, setRdp] = useState<HeadplaneRDP | null>(null);
  const [connected, setConnected] = useState(false);
  const [status, setStatus] = useState("Starting tunnel…");
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;

    loadHeadplaneRDPWASM().then((create) => {
      if (cancelled) return;

      setStatus("Joining Tailnet…");
      const instance = create({
        controlURL: node.controlURL,
        preAuthKey: node.preAuthKey,
        hostname: node.ephemeralHostname,
        onReady: () => {
          if (!cancelled) {
            setStatus(`Connecting to ${hostname} via RDP…`);
            setRdp(instance);
          }
        },
        onError: (msg) => {
          console.error("[rdp] IPN error:", msg);
          if (!cancelled) setError(`Tailnet error: ${msg}`);
        },
      });
    }).catch((err) => {
      if (!cancelled) setError(`Failed to load RDP module: ${err}`);
    });

    return () => {
      cancelled = true;
    };
  }, [node]);

  return (
    <div className="fixed inset-0 bg-black">
      {!connected && !error && (
        <div className="absolute inset-0 z-50 flex items-center justify-center">
          <div className="flex flex-col items-center gap-3">
            <Loader2 className="size-8 animate-spin text-mist-200" />
            <p className="text-sm text-mist-400">{status}</p>
          </div>
        </div>
      )}

      {error && (
        <div className="absolute inset-0 z-50 flex items-center justify-center">
          <div className="flex flex-col items-center gap-4 max-w-md text-center px-6">
            <p className="text-red-400 font-medium">RDP Connection Failed</p>
            <p className="text-sm text-mist-400 font-mono break-all">{error}</p>
            <button
              className="text-sm text-mist-300 underline"
              onClick={() => window.location.reload()}
            >
              Try again
            </button>
          </div>
        </div>
      )}

      {rdp && !error && (
        <RDPCanvas
          rdp={rdp}
          ipAddress={node.ipAddress}
          username={username}
          password={password}
          domain={domain}
          onConnected={() => setConnected(true)}
          onError={(msg) => setError(msg)}
        />
      )}
    </div>
  );
}
