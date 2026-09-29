// Command hp_server is the Go replacement for the Node production server.
//
// Phase 1 skeleton: it loads the same config surface as the TS loader,
// serves the SPA static bundle out of the client directory with the exact
// cache/HEAD/traversal semantics of runtime/http.ts, answers /healthz,
// honors the HEADPLANE_LISTEN_FILE Docker contract, and shuts down
// gracefully on SIGINT/SIGTERM with the 5 s force-exit guard.
//
// Later phases add: session/auth (2), OIDC (3), the Headscale JSON API (4),
// and the remaining endpoints (5).
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tale/headplane"
	"github.com/tale/headplane/internal/auth"
	"github.com/tale/headplane/internal/hsapi"
	"github.com/tale/headplane/internal/hscfg"
	"github.com/tale/headplane/internal/integration"
	"github.com/tale/headplane/internal/live"
	"github.com/tale/headplane/internal/rdpgw"
	"github.com/tale/headplane/internal/serverconfig"
)

// basenameEnv mirrors the build-time __INTERNAL_PREFIX contract from
// vite.config.ts: the URL prefix the app is served under.
const basenameEnv = "__INTERNAL_PREFIX"

const defaultBasename = "/admin"

// shutdownGracePeriod mirrors the 5 s force-exit guard in runtime/http.ts.
const shutdownGracePeriod = 5 * time.Second

func main() {
	clientDir := flag.String("client-dir", "build/client", "directory holding the SPA build output (build/client)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	basename := os.Getenv(basenameEnv)
	if basename == "" {
		basename = defaultBasename
	}
	if strings.HasSuffix(basename, "/") {
		logger.Error("URL prefix must not end with a slash", "prefix", basename)
		os.Exit(1)
	}

	cfg, err := serverconfig.Load("")
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	var tlsCert *tls.Certificate
	if cfg.Server.TLSCertPath != "" || cfg.Server.TLSKeyPath != "" {
		if cfg.Server.TLSCertPath == "" || cfg.Server.TLSKeyPath == "" {
			logger.Error("TLS misconfigured: both `server.tls_cert_path` and `server.tls_key_path` must be provided")
			os.Exit(1)
		}
		cert, err := tls.LoadX509KeyPair(cfg.Server.TLSCertPath, cfg.Server.TLSKeyPath)
		if err != nil {
			logger.Error("failed to read TLS material", "error", err)
			os.Exit(1)
		}
		tlsCert = &cert
	}

	srv := newServer(cfg, basename, *clientDir, logger)

	// Phase 2: session/auth. The DB file is shared with the Node server
	// (same schema); the byte-identical _hp_auth cookie scheme is what makes
	// sessions issued by either server validate on the other.
	headscaleAPIKey := cfg.Headscale.APIKey
	if headscaleAPIKey == "" && cfg.OIDC != nil {
		headscaleAPIKey = cfg.OIDC.HeadscaleAPIKey
	}
	db, err := auth.OpenDB(filepath.Join(cfg.Server.DataPath, "hp_persist.db"))
	if err != nil {
		logger.Error("failed to open auth database", "error", err)
		os.Exit(1)
	}
	// Same drizzle migrations the Node server applies on boot
	// (app/server/db/client.server.ts): a fresh data directory gets the
	// full schema, and this is a no-op on an existing database.
	if err := dbmigrate.Migrate(db); err != nil {
		logger.Error("failed to migrate auth database", "error", err)
		os.Exit(1)
	}
	authSvc := auth.NewService(db, cfg.Server.CookieSecret, headscaleAPIKey, auth.CookieOptions{
		Name:   "_hp_auth",
		Path:   basename,
		MaxAge: cfg.Server.CookieMaxAge,
		Secure: cfg.Server.CookieSecure,
		Domain: cfg.Server.CookieDomain,
	})
	authSvc.Start()
	srv.authSvc = authSvc

	// Phase 3: OIDC. newOidcService mirrors buildOidc in
	// app/server/context.ts, including the three disabled reasons; the
	// routes answer 501 "OIDC is unavailable: <reason>" when disabled.
	oidcSvc, oidcReason := newOidcService(cfg, basename, logger)
	srv.oidcSvc = oidcSvc
	srv.oidcDisabledReason = oidcReason
	if oidcSvc == nil {
		logger.Info("OIDC disabled", "reason", oidcReason)
	}

	// Phase 4: Headscale API layer + live store/SSE. The config file is read
	// once at startup and never watched or written, mirroring
	// getHeadscaleConfig.
	srv.hsCfg = hscfg.Load(cfg.Headscale.ConfigPath, cfg.Headscale.DNSRecordsPath, logger)
	logger.Info("headscale config", "access", string(srv.hsCfg.Access()))

	var defaultHSClient *hsapi.Client
	if headscaleAPIKey != "" {
		defaultHSClient = hsapi.NewClient(cfg.Headscale.URL, headscaleAPIKey,
			cfg.Headscale.TLSCertPath, srv.getHsCaps(), logger)
	}
	srv.liveStore = live.NewStore(logger, defaultHSClient)

	// /healthz probes Headscale's unauthenticated /health, regaining the
	// 200/500 semantics of the TS route.
	healthClient := hsapi.NewClient(cfg.Headscale.URL, "", cfg.Headscale.TLSCertPath, srv.getHsCaps(), logger)
	srv.healthCheck = healthClient.Health
	srv.hsHealth = healthClient.Health

	// Phase 5: restart integration (docker/kubernetes/proc). Load picks the
	// single enabled one, logging when none or several are enabled.
	if ic := cfg.Integration; ic != nil {
		var icfg integration.Config
		if ic.Docker != nil {
			icfg.Docker = &integration.DockerConfig{
				Enabled:        ic.Docker.Enabled,
				ContainerName:  ic.Docker.ContainerName,
				ContainerLabel: ic.Docker.ContainerLabel,
				Socket:         ic.Docker.Socket,
			}
		}
		if ic.Kubernetes != nil {
			icfg.Kubernetes = &integration.KubernetesConfig{
				Enabled:          ic.Kubernetes.Enabled,
				PodName:          ic.Kubernetes.PodName,
				ValidateManifest: ic.Kubernetes.ValidateManifest,
			}
		}
		if ic.Proc != nil {
			icfg.Proc = &integration.ProcConfig{Enabled: ic.Proc.Enabled}
		}
		srv.integration = integration.Load(icfg, logger)
	}

	// Phase 5: RDP gateway webhook client. The TS schema defaults the
	// section to enabled when present.
	if cfg.RDPGateway.IsEnabled() {
		srv.rdpGw = rdpgw.NewClient(cfg.RDPGateway.WebhookURL, cfg.RDPGateway.WebhookToken, logger)
		logger.Info("RDP gateway enabled", "webhook", cfg.RDPGateway.WebhookURL)
	}

	// Version detection with the 30 s retry loop from
	// app/server/headscale/api/index.ts: capabilities stay permissive until
	// /version answers, then tighten and refresh the live store's client.
	// The key is also stashed for the agent pre-detection disabled reason.
	srv.headscaleAPIKey = headscaleAPIKey
	hsDone := make(chan struct{})
	go srv.detectHeadscaleVersion(headscaleAPIKey, hsDone)

	prevShutdown := srv.onShutdown
	srv.onShutdown = func() {
		close(hsDone)
		srv.liveStore.Dispose()
		// Phase 5: stop the agent sync loop and child process.
		if mgr, _ := srv.agentManager(); mgr != nil {
			mgr.Dispose()
		}
		if prevShutdown != nil {
			prevShutdown()
		}
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("failed to listen", "addr", addr, "error", err)
		os.Exit(1)
	}

	scheme := "http"
	if tlsCert != nil {
		scheme = "https"
		listener = tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{*tlsCert}})
	}
	logger.Info("listening", "scheme", scheme, "addr", listener.Addr().String())

	// HEADPLANE_LISTEN_FILE is a Docker-specific contract: the Dockerfile
	// sets it so the bundled hp_healthcheck binary can discover the URL to
	// probe. The URL is written once the server is accepting connections.
	if listenPath := os.Getenv(serverconfig.EnvListenFile); listenPath != "" {
		url := fmt.Sprintf("%s://127.0.0.1:%d%s/healthz\n", scheme, boundPort(listener), basename)
		if err := os.WriteFile(listenPath, []byte(url), 0o644); err != nil {
			logger.Error("failed to write listen file", "path", listenPath, "error", err)
		}
	}

	httpServer := &http.Server{Handler: srv}

	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	waitForSignal(logger)
	logger.Info("shutting down")

	// Force exit if connections don't drain in time (mirrors the TS guard).
	go func() {
		time.Sleep(shutdownGracePeriod)
		os.Exit(0)
	}()

	if err := httpServer.Shutdown(context.Background()); err != nil {
		logger.Error("error during shutdown", "error", err)
	}
	if hook := srv.onShutdown; hook != nil {
		hook()
	}
	authSvc.Stop()
	db.Close()
	os.Exit(0)
}

// boundPort returns the actual port the listener bound (the config value in
// the normal case; the ephemeral port when server.port is 0).
func boundPort(l net.Listener) int {
	if addr, ok := l.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

func waitForSignal(logger *slog.Logger) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("received signal", "signal", sig.String())
	signal.Stop(sigCh)
}
