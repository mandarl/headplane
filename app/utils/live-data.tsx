import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { useRevalidator } from "react-router";

const LiveDataContext = createContext({
  paused: false,
  setPaused: (_: boolean) => {},
});

interface LiveDataProps {
  children: React.ReactNode;
}

type Versions = Record<string, string>;
interface ChangedEvent {
  resource: string;
  version: string;
}

export function LiveDataProvider({ children }: LiveDataProps) {
  const revalidator = useRevalidator();
  const [paused, setPaused] = useState(false);

  // This ref is a bit sus but it's needed to ensure the SSE handshake does
  // not re-establish on every revalidation. The SSE stream is always stable
  const revalidatorRef = useRef(revalidator);
  revalidatorRef.current = revalidator;

  // Highest version seen per resource. Versions are a monotonic counter as
  // decimal strings, so this only ever moves forward — which lets us
  // ignore the ring the server replays on (re)connect instead of
  // revalidating once per replayed event.
  const versionsRef = useRef<Versions>({});
  const hasHelloRef = useRef(false);
  const isTabDirtyRef = useRef(false);

  // noteVersion records a version if it is newer than what we've seen for
  // the resource, returning whether it advanced.
  const noteVersion = useCallback((resource: string, version: string): boolean => {
    const current = versionsRef.current[resource];
    if (current !== undefined && Number(version) <= Number(current)) {
      return false;
    }
    versionsRef.current = { ...versionsRef.current, [resource]: version };
    return true;
  }, []);

  const eventSourceRef = useRef<EventSource | null>(null);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const backoffRef = useRef(1000);
  const lastRevalidateRef = useRef(0);
  const trailingTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Rate-limit revalidations to at most one per this window. A reconnect
  // replays every buffered `changed` event (Last-Event-ID resumption), and
  // a chatty/misbehaving server could emit them continuously; without a
  // floor here that becomes a tight revalidate → refetch loop. A request
  // that arrives inside the window schedules one trailing revalidation so
  // no change is lost.
  const MIN_REVALIDATE_INTERVAL_MS = 2000;

  const revalidateIfIdle = useCallback(() => {
    const run = () => {
      if (revalidatorRef.current.state === "idle") {
        lastRevalidateRef.current = Date.now();
        revalidatorRef.current.revalidate();
      }
    };
    const elapsed = Date.now() - lastRevalidateRef.current;
    if (elapsed >= MIN_REVALIDATE_INTERVAL_MS) {
      run();
      return;
    }
    if (trailingTimerRef.current === null) {
      trailingTimerRef.current = setTimeout(() => {
        trailingTimerRef.current = null;
        run();
      }, MIN_REVALIDATE_INTERVAL_MS - elapsed);
    }
  }, []);

  // SSE connection
  useEffect(() => {
    if (paused) {
      if (eventSourceRef.current) {
        eventSourceRef.current.close();
        eventSourceRef.current = null;
      }

      return;
    }

    function connect() {
      const sse = new EventSource(`${__PREFIX__}/events/live`);
      eventSourceRef.current = sse;

      sse.addEventListener("hello", (e) => {
        backoffRef.current = 1000;
        try {
          const map = JSON.parse(e.data) as Versions;
          let advanced = false;
          for (const [resource, version] of Object.entries(map)) {
            if (noteVersion(resource, version)) {
              advanced = true;
            }
          }
          // First connect: the route loaders have already run, so there's
          // nothing to do. A reconnect where a version advanced means we
          // missed changes while offline — one catch-up revalidation
          // (the replayed ring is then all <= what we now hold).
          if (hasHelloRef.current && advanced) {
            revalidateIfIdle();
          }
          hasHelloRef.current = true;
        } catch {}
      });

      sse.addEventListener("changed", (e) => {
        try {
          const data = JSON.parse(e.data) as ChangedEvent;
          if (!noteVersion(data.resource, data.version)) {
            return; // already seen (or a replayed ring entry) — ignore
          }

          if (document.visibilityState !== "visible") {
            isTabDirtyRef.current = true;
            return;
          }

          revalidateIfIdle();
        } catch {}
      });

      sse.onerror = () => {
        sse.close();
        eventSourceRef.current = null;
        const delay = backoffRef.current;
        backoffRef.current = Math.min(delay * 2, 30_000);
        reconnectTimer.current = setTimeout(connect, delay);
      };
    }

    connect();
    return () => {
      if (eventSourceRef.current) {
        eventSourceRef.current.close();
        eventSourceRef.current = null;
      }

      if (reconnectTimer.current) {
        clearTimeout(reconnectTimer.current);
        reconnectTimer.current = null;
      }

      if (trailingTimerRef.current) {
        clearTimeout(trailingTimerRef.current);
        trailingTimerRef.current = null;
      }
    };
  }, [paused, revalidateIfIdle, noteVersion]);

  // If the tab becomes visible and is marked dirty, revalidate
  useEffect(() => {
    const visibilityCallback = () => {
      if (document.visibilityState === "visible" && isTabDirtyRef.current) {
        isTabDirtyRef.current = false;
        revalidateIfIdle();
      }
    };

    document.addEventListener("visibilitychange", visibilityCallback);
    return () => {
      document.removeEventListener("visibilitychange", visibilityCallback);
    };
  }, [revalidateIfIdle]);

  // Force a revalidation when the app comes back online
  useEffect(() => {
    window.addEventListener("online", revalidateIfIdle);
    return () => {
      window.removeEventListener("online", revalidateIfIdle);
    };
  }, [revalidateIfIdle]);

  return (
    <LiveDataContext.Provider value={{ paused, setPaused }}>{children}</LiveDataContext.Provider>
  );
}

export function useLiveData() {
  const context = useContext(LiveDataContext);
  return {
    pause: () => context.setPaused(true),
    resume: () => context.setPaused(false),
  };
}
