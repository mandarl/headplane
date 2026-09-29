package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeProc builds a fake /proc tree: pid -> (comm, cmdline).
func fakeProc(t *testing.T, procs map[string][2]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, files := range procs {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(files[0]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(files[1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestFindHeadscaleServe(t *testing.T) {
	root := fakeProc(t, map[string][2]string{
		"1":  {"bash", "bash\x00"},
		"42": {"headscale", "headscale\x00serve\x00"},
		"99": {"headscale", "headscale\x00--config\x00/etc/headscale/config.yaml\x00"},
		// non-numeric dirs are ignored
		"self": {"headscale", "headscale\x00serve\x00"},
	})

	pid, err := findHeadscaleServe(root)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 42 {
		t.Fatalf("expected pid 42, got %d", pid)
	}
}

func TestFindHeadscaleServeNotFound(t *testing.T) {
	root := fakeProc(t, map[string][2]string{
		"1": {"bash", "bash\x00"},
		"7": {"headscale", "headscale\x00version\x00"},
	})
	pid, err := findHeadscaleServe(root)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 0 {
		t.Fatalf("expected pid 0, got %d", pid)
	}
}

func TestFindHeadscaleServeMissingRoot(t *testing.T) {
	if _, err := findHeadscaleServe(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing proc root")
	}
}

func TestProcIsAvailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("proc scan is linux-only")
	}
	root := fakeProc(t, map[string][2]string{
		"4242": {"headscale", "/usr/bin/headscale\x00serve\x00"},
	})
	p := newProcIntegration(&ProcConfig{Enabled: true}, testLogger(), WithProcRoot(root))
	if !p.IsAvailable() {
		t.Fatal("expected integration to be available")
	}
	if p.pid != 4242 {
		t.Fatalf("unexpected pid: %d", p.pid)
	}
	if p.Name() != "Native Linux (/proc)" {
		t.Fatalf("unexpected name: %q", p.Name())
	}
}

func TestProcIsAvailableNotFound(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("proc scan is linux-only")
	}
	root := fakeProc(t, map[string][2]string{
		"1": {"init", "init\x00"},
	})
	p := newProcIntegration(&ProcConfig{Enabled: true}, testLogger(), WithProcRoot(root))
	if p.IsAvailable() {
		t.Fatal("expected integration to be unavailable")
	}
}

// TestSignalAndWaitHealthyHealthy signals a real child process and gets an
// immediate healthy report.
func TestSignalAndWaitHealthyHealthy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SIGHUP test is linux-only")
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	calls := 0
	if err := signalAndWaitHealthy(cmd.Process.Pid, func() bool {
		calls++
		return true
	}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 health call, got %d", calls)
	}
	// The child should be gone (SIGHUP's default disposition terminates).
	_ = cmd.Wait()
}

// TestSignalAndWaitHealthyNeverHealthy exercises the retry loop with sleeps
// disabled; the child ignores SIGHUP so the signal itself succeeds.
func TestSignalAndWaitHealthyNeverHealthy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SIGHUP test is linux-only")
	}
	noSleep(t)
	// `sleep` with SIGHUP ignored via trap so signaling succeeds but the
	// process stays alive; health never reports healthy.
	cmd := exec.Command("sh", "-c", "trap '' HUP; sleep 60")
	if err := cmd.Start(); err != nil {
		t.Skipf("sh not available: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	calls := 0
	err := signalAndWaitHealthy(cmd.Process.Pid, func() bool {
		calls++
		return false
	})
	if err == nil {
		t.Fatal("expected error when health never reports healthy")
	}
	if calls != 10 {
		t.Fatalf("expected 10 health calls, got %d", calls)
	}
}

// TestSignalAndWaitHealthyBadPid mirrors the TS kill-failure path.
func TestSignalAndWaitHealthyBadPid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SIGHUP test is linux-only")
	}
	// A pid that cannot exist: kill fails -> error without any health call.
	if err := signalAndWaitHealthy(1<<30, func() bool {
		t.Fatal("health should not be called")
		return true
	}); err == nil {
		t.Fatal("expected error for nonexistent pid")
	}
}

func TestProcOnConfigChangeNoPid(t *testing.T) {
	p := newProcIntegration(&ProcConfig{Enabled: true}, testLogger())
	// pid 0 -> no-op, mirrors the TS early return.
	if err := p.OnConfigChange(func() bool { return true }); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}
