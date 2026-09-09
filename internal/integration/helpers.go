package integration

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// sleep is time.Sleep, overridable by tests to skip retry delays.
var sleep = time.Sleep

// findHeadscaleServe does a two-stage scan of the proc filesystem to find
// the headscale process running a "serve" subcommand. It first scans all
// processes' comm files to find headscale processes, then checks their
// cmdline files to see if "serve" is the second argument.
//
// procRoot is the proc filesystem root (normally "/proc"); it is a
// parameter (rather than hardcoded) so tests can point it at a fake tree.
//
// Returns the PID of the headscale serve process, or 0 when not found.
// A non-nil error means the scan itself failed (e.g. procRoot unreadable).
func findHeadscaleServe(procRoot string) (int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, err
	}

	var headscalePids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // not a numeric dir
		}
		comm, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "comm"))
		if err != nil {
			continue // process may have exited between stages
		}
		if strings.TrimSpace(string(comm)) == "headscale" {
			headscalePids = append(headscalePids, pid)
		}
	}

	if len(headscalePids) == 0 {
		return 0, nil
	}

	pkgLogger.Debug("Found headscale process(es), checking for serve",
		"count", len(headscalePids))
	for _, pid := range headscalePids {
		cmdline, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
		if err != nil {
			continue // process may have exited between stages
		}
		var args []string
		for _, arg := range strings.Split(string(cmdline), "\x00") {
			if arg != "" {
				args = append(args, arg)
			}
		}
		if len(args) > 1 && args[1] == "serve" {
			return pid, nil
		}
	}

	return 0, nil
}

// signalAndWaitHealthy sends SIGHUP to the headscale process and waits for
// it to become healthy: it sleeps 1s, then makes up to 10 attempts 1s apart
// calling health(). Returns nil on the first healthy report, else an error.
func signalAndWaitHealthy(pid int, health func() bool) error {
	if err := sendSIGHUP(pid); err != nil {
		pkgLogger.Error("Failed to send SIGHUP", "pid", pid, "error", err)
		return fmt.Errorf("failed to send SIGHUP to PID %d: %w", pid, err)
	}
	pkgLogger.Info("Sent SIGHUP to Headscale", "pid", pid)

	sleep(time.Second)
	for attempt := 1; attempt <= 10; attempt++ {
		if health() {
			pkgLogger.Info("Headscale is healthy after restart")
			return nil
		}
		if attempt < 10 {
			sleep(time.Second)
		}
	}

	pkgLogger.Error("Headscale did not become healthy after 10 attempts")
	return errors.New("headscale did not become healthy after 10 attempts")
}
