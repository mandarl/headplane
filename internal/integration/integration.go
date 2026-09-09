// Package integration restarts or reloads Headscale after a headplane
// config change, via one of three integrations: Docker (restart the
// Headscale container), Kubernetes (SIGHUP the headscale serve process in
// a pod with a shared process namespace), or a native /proc scan on Linux.
//
// This is a port of app/server/config/integration/ (abstract.ts, docker.ts,
// kubernetes.ts, proc.ts, proc-helper.ts, index.ts).
package integration

import (
	"log/slog"
	"runtime"
)

// DockerConfig mirrors the headplane.yaml `integration.docker` section.
type DockerConfig struct {
	Enabled bool
	// ContainerName optionally selects the container by name, overriding
	// the label selector (legacy support).
	ContainerName string
	// ContainerLabel selects the container by label.
	// Defaults to "me.tale.headplane.target=headscale" when empty.
	ContainerLabel string
	// Socket is the Docker daemon address, either
	// "unix:///var/run/docker.sock" or "tcp://host:port".
	// Defaults to "unix:///var/run/docker.sock" when empty.
	Socket string
}

// KubernetesConfig mirrors the headplane.yaml `integration.kubernetes` section.
type KubernetesConfig struct {
	Enabled bool
	PodName string
	// ValidateManifest controls the strict pod manifest check
	// (spec.shareProcessNamespace must be present and true).
	// Nil means true, mirroring the TS schema default.
	ValidateManifest *bool
}

// ProcConfig mirrors the headplane.yaml `integration.proc` section.
type ProcConfig struct {
	Enabled bool
}

// Config mirrors the headplane.yaml `integration` section (the subset this
// package needs). The parent wires serverconfig -> this Config.
type Config struct {
	Docker     *DockerConfig
	Kubernetes *KubernetesConfig
	Proc       *ProcConfig
}

// Integration restarts/reloads Headscale after a config change.
type Integration interface {
	Name() string
	// IsAvailable probes the environment (idempotent; caches what it needs).
	IsAvailable() bool
	// OnConfigChange restarts or reloads Headscale, then waits until
	// health() reports healthy (or gives up). health probes Headscale.
	OnConfigChange(health func() bool) error
}

// pkgLogger is set by Load so the shared proc helpers (whose signatures are
// fixed to the TS port) can log. Defaults to slog.Default() when Load has
// not run yet (e.g. in tests that build integrations directly).
var pkgLogger = slog.Default()

// Load picks the single enabled integration (nil when none enabled).
// More than one enabled -> nil plus a logged error
// ("Multiple integrations enabled, please pick one only").
// When the chosen integration's IsAvailable() is false -> nil.
func Load(cfg Config, logger *slog.Logger) Integration {
	if logger == nil {
		logger = slog.Default()
	}
	pkgLogger = logger

	enabled := 0
	if cfg.Docker != nil && cfg.Docker.Enabled {
		enabled++
	}
	if cfg.Kubernetes != nil && cfg.Kubernetes.Enabled {
		enabled++
	}
	if cfg.Proc != nil && cfg.Proc.Enabled {
		enabled++
	}

	if enabled == 0 {
		logger.Debug("No integrations enabled")
		return nil
	}
	if enabled > 1 {
		// Note: the TS (index.ts) only catches all three being enabled at
		// once (docker?.enabled && k8s?.enabled && proc?.enabled), so any
		// pair slips through and the first match wins. We intentionally
		// enforce the documented "pick one only" rule for every pair.
		logger.Error("Multiple integrations enabled, please pick one only")
		return nil
	}

	var in Integration
	switch {
	case cfg.Docker != nil && cfg.Docker.Enabled:
		logger.Info("Using Docker integration")
		in = newDockerIntegration(cfg.Docker, logger)
	case cfg.Kubernetes != nil && cfg.Kubernetes.Enabled:
		logger.Info("Using Kubernetes integration")
		in = newKubernetesIntegration(cfg.Kubernetes, logger)
	case cfg.Proc != nil && cfg.Proc.Enabled:
		logger.Info("Using Proc integration")
		in = newProcIntegration(cfg.Proc, logger)
	}

	if !in.IsAvailable() {
		logger.Error("Integration is not available", "name", in.Name())
		return nil
	}
	return in
}

// linuxOnly reports whether the current platform can use /proc-based
// integrations.
func linuxOnly() bool {
	return runtime.GOOS == "linux"
}
