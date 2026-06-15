import { useEffect, useRef, useState } from "react";
import { Loader2 } from "lucide-react";

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
      onDisconnect: () => {},
      onError: (msg) => {
        console.error("[rdp] session error:", msg);
        onError(msg);
      },
    });

    sessionRef.current = session;

    function onKeyDown(e: KeyboardEvent) {
      e.preventDefault();
      sessionRef.current?.sendKey(e.keyCode, true);
    }
    function onKeyUp(e: KeyboardEvent) {
      e.preventDefault();
      sessionRef.current?.sendKey(e.keyCode, false);
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
      className="block h-screen w-screen cursor-none outline-none"
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
