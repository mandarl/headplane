package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tale/headplane/internal/auth"
	"github.com/tale/headplane/internal/hsapi"
	"github.com/tale/headplane/internal/hscfg"
	"github.com/tale/headplane/internal/live"
	"github.com/tale/headplane/internal/oidc"
	"github.com/tale/headplane/internal/serverconfig"
)

// contentTypes pins the MIME types for extensions the SPA bundle uses.
// Go's mime.TypeByExtension falls back to the OS table, which may not know
// .wasm; the TS server used the `mime` npm package, so pin the important
// ones here for determinism. Mirrors server/interim.mjs.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".webp":  "image/webp",
	".woff2": "font/woff2",
	".wasm":  "application/wasm",
	".txt":   "text/plain; charset=utf-8",
}

func contentTypeFor(name string) string {
	if ct, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// HealthChecker reports whether the deployment is healthy. Phase 1 wires the
// trivial always-healthy check; Phase 4 replaces it with the Headscale probe
// so /healthz regains the 200/500 semantics of the TS route.
type HealthChecker func() bool

// Server is the Phase 1 HTTP server: static SPA bundle + /healthz.
type Server struct {
	cfg         *serverconfig.Config
	basename    string
	clientDir   string
	logger      *slog.Logger
	healthCheck HealthChecker
	authSvc     *auth.Service

	// Phase 3: OIDC. oidcSvc is nil when OIDC is disabled, in which case
	// oidcDisabledReason carries the TS "OIDC is unavailable: <reason>".
	oidcSvc            *oidc.Service
	oidcDisabledReason string

	// Phase 4: Headscale. hsCfg is the read-only Headscale config (nil-safe
	// via its methods); liveStore is the versioned nodes/users cache feeding
	// the API and the SSE stream. hsCapsMu guards the version-derived
	// capability flags, which start permissive and tighten once /version
	// detection succeeds (mirroring index.ts's detect loop).
	hsCfg     *hscfg.Config
	liveStore *live.Store
	hsCapsMu  sync.RWMutex
	hsCaps    hsapi.Capabilities

	// Phase 5: agent manager (lazy), RDP gateway webhook client,
	// restart integration, detected Headscale version, health probe.
	phase5State

	// onShutdown runs after the listener drains, before process exit.
	// Phase 1 has no long-lived resources; later phases hook in here.
	onShutdown func()
}

func newServer(cfg *serverconfig.Config, basename, clientDir string, logger *slog.Logger) *Server {
	abs, err := filepath.Abs(clientDir)
	if err != nil {
		abs = clientDir
	}
	return &Server{
		cfg:         cfg,
		basename:    basename,
		clientDir:   abs,
		logger:      logger.With("component", "server"),
		healthCheck: func() bool { return true },
		// Capabilities start permissive (as if the newest known
		// Headscale) until /version detection succeeds, mirroring the
		// TS detect loop in app/server/headscale/api/index.ts.
		hsCaps: hsapi.CapabilitiesFor(hsapi.ParseServerVersion("unreachable")),
	}
}

// getHsCaps returns the current version-derived capability flags.
func (s *Server) getHsCaps() hsapi.Capabilities {
	s.hsCapsMu.RLock()
	defer s.hsCapsMu.RUnlock()
	return s.hsCaps
}

// setHsCaps replaces the capability flags (called when /version detection
// succeeds) and refreshes the live store's client so polling uses them.
func (s *Server) setHsCaps(caps hsapi.Capabilities) {
	s.hsCapsMu.Lock()
	s.hsCaps = caps
	s.hsCapsMu.Unlock()
}

// ServeHTTP is a single hand-rolled router. It works from the raw
// RequestURI (like the TS code works from req.url) instead of a ServeMux so
// that encoded traversal sequences reach our own guard and answer 404
// instead of being sanitized into a 301 by the mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rawPath := r.RequestURI
	if i := strings.IndexByte(rawPath, '?'); i >= 0 {
		rawPath = rawPath[:i]
	}
	pathname, err := url.PathUnescape(rawPath)
	if err != nil {
		s.notFound(w)
		return
	}

	// 1. `${basename}` → 302 to `${basename}/` (mirrors runtime/http.ts,
	// preserving the query string).
	if pathname == s.basename {
		query := ""
		if r.URL.RawQuery != "" {
			query = "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", s.basename+"/"+query)
		w.WriteHeader(http.StatusFound)
		return
	}

	// 2. Health check.
	if pathname == s.basename+"/healthz" {
		s.serveHealthz(w, r)
		return
	}

	// 3. Auth endpoints (Phase 2). These stay server-driven in the SPA: the
	// browser never sees the Headscale API key or the session internals.
	if pathname == s.basename+"/login" && r.Method == http.MethodPost {
		s.handleLogin(w, r)
		return
	}
	if pathname == s.basename+"/logout" {
		s.handleLogout(w, r)
		return
	}
	if pathname == s.basename+"/oidc/start" && r.Method == http.MethodGet {
		s.handleOidcStart(w, r)
		return
	}
	if pathname == s.basename+"/oidc/callback" && r.Method == http.MethodGet {
		s.handleOidcCallback(w, r)
		return
	}

	// 4. Phase 4: the versioned JSON API the SPA talks to, plus the
	// live-update SSE stream. These sit before the static fallback so
	// /api/v1/* and /events/live never resolve to the SPA shell.
	if rest, ok := strings.CutPrefix(pathname, s.basename+apiPrefix); ok {
		s.serveAPIv1(w, r, rest)
		return
	}
	if pathname == s.basename+"/events/live" {
		s.handleLive(w, r)
		return
	}

	// 4b. Phase 5: server-driven utility routes outside /api/v1 (mirrors
	// the ...prefix("/api", ...) block in app/routes.ts).
	if pathname == s.basename+"/api/rdp-gateway" {
		s.handleRdpGateway(w, r)
		return
	}
	if pathname == s.basename+"/api/info" {
		s.handleInfo(w, r)
		return
	}

	// 5. Static assets + SPA fallback for GET/HEAD under the basename.
	if strings.HasPrefix(pathname, s.basename+"/") {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			s.serveStatic(w, r, pathname)
			return
		}
	}

	s.notFound(w)
}

func (s *Server) serveHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.notFound(w)
		return
	}
	healthy := true
	if s.healthCheck != nil {
		healthy = s.healthCheck()
	}
	status := "OK"
	code := http.StatusOK
	if !healthy {
		status = "ERROR"
		code = http.StatusInternalServerError
	}
	body, _ := json.Marshal(map[string]string{"status": status})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// serveStatic serves a file from the client directory with the production
// cache semantics from runtime/http.ts, or falls back to the SPA shell.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, pathname string) {
	rel := strings.TrimPrefix(pathname, s.basename+"/")
	if rel == "" || strings.HasSuffix(rel, "/") {
		s.serveIndex(w, r)
		return
	}

	// Traversal attempts 404 — never SPA fallback (matches runtime/http.ts
	// and server/interim.mjs).
	cleaned := path.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		s.notFound(w)
		return
	}
	file := filepath.Join(s.clientDir, filepath.FromSlash(cleaned))
	if file != s.clientDir && !strings.HasPrefix(file, s.clientDir+string(filepath.Separator)) {
		s.notFound(w)
		return
	}

	st, err := os.Stat(file)
	if err != nil || !st.Mode().IsRegular() {
		// Missing file: GET falls through to the SPA shell (client-side
		// routes), HEAD has no shell to serve.
		if r.Method == http.MethodGet {
			s.serveIndex(w, r)
		} else {
			s.notFound(w)
		}
		return
	}

	// isAsset is computed from the original (uncleaned) pathname, exactly
	// like the TS handler.
	if strings.HasPrefix(pathname, s.basename+"/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.Header().Set("Content-Type", contentTypeFor(file))
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	f, err := os.Open(file)
	if err != nil {
		// Headers already sent; mirror the TS handler's destroy-on-error.
		s.logger.Error("static read failed", "file", file, "error", err)
		return
	}
	defer f.Close()
	if _, err := io.Copy(w, f); err != nil {
		s.logger.Error("static write failed", "file", file, "error", err)
	}
}

// serveIndex serves the SPA shell. It must never be cached: it references
// hashed asset filenames.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	file := filepath.Join(s.clientDir, "index.html")
	st, err := os.Stat(file)
	if err != nil || !st.Mode().IsRegular() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"SPA build not found"}`))
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("Last-Modified", st.ModTime().UTC().Format(time.RFC1123))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	f, err := os.Open(file)
	if err != nil {
		s.logger.Error("index read failed", "error", err)
		return
	}
	defer f.Close()
	_, _ = io.Copy(w, f)
}

func (s *Server) notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"Not found"}`))
}
