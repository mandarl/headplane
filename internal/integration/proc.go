package integration

import (
	"log/slog"
)

const defaultProcRoot = "/proc"

type procIntegration struct {
	logger   *slog.Logger
	procRoot string
	pid      int
}

// ProcOption customizes a procIntegration. The only knob is the proc
// filesystem root, which exists so tests can point the scan at a fake /proc
// tree instead of the real one.
type ProcOption func(*procIntegration)

// WithProcRoot overrides the proc filesystem root (default "/proc").
// Test-only: production code should not set this.
func WithProcRoot(root string) ProcOption {
	return func(p *procIntegration) { p.procRoot = root }
}

func newProcIntegration(cfg *ProcConfig, logger *slog.Logger, opts ...ProcOption) *procIntegration {
	p := &procIntegration{logger: logger, procRoot: defaultProcRoot}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *procIntegration) Name() string { return "Native Linux (/proc)" }

func (p *procIntegration) IsAvailable() bool {
	if !linuxOnly() {
		p.logger.Error("/proc is only available on Linux")
		return false
	}

	pid, err := findHeadscaleServe(p.procRoot)
	if err != nil {
		p.logger.Error("Failed to scan /proc", "error", err)
		return false
	}
	if pid == 0 {
		p.logger.Error("Could not find headscale serve process")
		return false
	}

	p.pid = pid
	p.logger.Info("Found headscale serve", "pid", pid)
	return true
}

func (p *procIntegration) OnConfigChange(health func() bool) error {
	if p.pid == 0 {
		return nil
	}
	return signalAndWaitHealthy(p.pid, health)
}
