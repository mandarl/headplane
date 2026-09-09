package main

// Phase 4: the live-update SSE stream, the Go port of
// app/routes/util/live.ts.
//
// Protocol (unchanged from the TS):
//   event: hello     data: {"nodes": "<version>", "users": "<version>"}
//   event: changed    data: {"resource": "<key>", "version": "<version>"}
//   : heartbeat                        (every 30 s)
// Plus the Phase 4 addition: every changed event carries `id: <n>` with a
// globally monotonic id, so the browser's native EventSource resends
// Last-Event-ID on reconnect and the server replays missed events
// (dedupe on the client is by resource/version, as before).
//
// Headers mirror the TS exactly, including X-Accel-Buffering: no.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// sseHeartbeatInterval mirrors the TS 30 s heartbeat.
const sseHeartbeatInterval = 30 * time.Second

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	api, _, err := s.hsClientFor(r)
	if err != nil {
		if err == errNeedAuth {
			writeError(w, http.StatusUnauthorized, "Authentication required.")
		} else {
			writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		}
		return
	}

	// Load both resources before opening the stream, like the TS loader.
	if _, _, err := s.liveSnapshots(r.Context(), api); err != nil {
		writeAPIError(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "Streaming is not supported.")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	writeEvent := func(id uint64, name string, data any) {
		var sb strings.Builder
		if id > 0 {
			fmt.Fprintf(&sb, "id: %d\n", id)
		}
		if name != "" {
			fmt.Fprintf(&sb, "event: %s\n", name)
		}
		raw, _ := json.Marshal(data)
		sb.WriteString("data: ")
		sb.Write(raw)
		sb.WriteString("\n\n")
		_, _ = io.WriteString(w, sb.String())
		flusher.Flush()
	}

	// Subscribe BEFORE snapshotting versions/replay so no change can slip
	// through the gap. A change arriving between the replay snapshot and
	// the subscription is caught live; a replayed event may duplicate a
	// live notification, which the SPA dedupes by resource+version.
	events, unsubscribe := s.liveStore.Subscribe()
	defer unsubscribe()

	// The hello carries the current version map; it always comes first so
	// the client can synchronize.
	writeEvent(0, "hello", s.liveStore.Versions())

	// Then replay anything missed since the client's last event (native
	// EventSource resends Last-Event-ID automatically on reconnect). On a
	// fresh connect there is no Last-Event-ID and nothing to catch up —
	// the hello already carries the current versions — so skip the ring
	// rather than dumping up to replayCap stale events the client just
	// dedupes away.
	if hdr := strings.TrimSpace(r.Header.Get("Last-Event-ID")); hdr != "" {
		since, _ := strconv.ParseUint(hdr, 10, 64)
		for _, ev := range s.liveStore.Replay(since) {
			writeEvent(ev.ID, "changed", map[string]string{"resource": ev.Resource, "version": ev.Version})
		}
	}

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				// Evicted as a slow consumer (or store disposed): end the
				// stream and let the client reconnect fresh.
				return
			}
			writeEvent(ev.ID, "changed", map[string]string{"resource": ev.Resource, "version": ev.Version})
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
