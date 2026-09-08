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
	"strings"
	"syscall"
	"time"

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
