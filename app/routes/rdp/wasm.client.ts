const WASM_MODULE_URL = `${__PREFIX__}/hp_rdp.wasm`;
const WASM_HELPER_URL = `${__PREFIX__}/wasm_exec.js`;

declare global {
  type HeadplaneRDPFactory = (config: HeadplaneRDPConfig) => HeadplaneRDP;
  var __hp_rdp_resolve: ((factory: HeadplaneRDPFactory) => void) | undefined;

  var Go: {
    new (): {
      importObject: WebAssembly.Imports;
      run(instance: WebAssembly.Instance): Promise<void>;
    };
  };
}

interface HeadplaneRDPConfig {
  controlURL: string;
  preAuthKey: string;
  hostname: string;
  onReady: () => void;
  onError?: (message: string) => void;
}

export interface HeadplaneRDP {
  openSession(config: RDPSessionConfig): RDPSession;
}

export interface RDPSessionConfig {
  ipAddress: string;
  username: string;
  password: string;
  domain?: string;
  width: number;
  height: number;
  colorDepth?: number;
  onUpdate: (x: number, y: number, w: number, h: number, pixels: Uint8ClampedArray) => void;
  onConnect: () => void;
  onDisconnect: () => void;
  onError: (msg: string) => void;
}

export interface RDPSession {
  sendKey(scancode: number, down: boolean): void;
  sendMouse(button: number, x: number, y: number, down: boolean): void;
  close(): void;
}

let resolvedFactory: Promise<HeadplaneRDPFactory> | null = null;

function loadGoHelper(): Promise<void> {
  if (typeof globalThis.Go !== "undefined") {
    return Promise.resolve();
  }

  return new Promise((resolve, reject) => {
    const script = document.createElement("script");
    script.src = WASM_HELPER_URL;
    script.crossOrigin = "anonymous";
    script.onload = () => resolve();
    script.onerror = () => reject(new Error("Failed to load Go WASM helper"));
    document.head.appendChild(script);
  });
}

export function loadHeadplaneRDPWASM(): Promise<HeadplaneRDPFactory> {
  if (!resolvedFactory) {
    // Assign before any await so concurrent callers get the same promise.
    resolvedFactory = (async () => {
      await loadGoHelper();
      const go = new Go();
      const result = await WebAssembly.instantiateStreaming(fetch(WASM_MODULE_URL), go.importObject);
      return new Promise<HeadplaneRDPFactory>((resolve) => {
        globalThis.__hp_rdp_resolve = resolve;
        go.run(result.instance);
      });
    })();
  }

  return resolvedFactory;
}
