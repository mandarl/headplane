package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func noSleep(t *testing.T) {
	t.Helper()
	old := sleep
	sleep = func(time.Duration) {}
	t.Cleanup(func() { sleep = old })
}

func TestCompareAPIVersions(t *testing.T) {
	cases := []struct {
		current, required string
		want              int
		wantErr           bool
	}{
		{"1.44", "1.44", 0, false},
		{"1.45", "1.44", 1, false},
		{"1.43", "1.44", -1, false},
		{"2.0", "1.44", 1, false},
		{"1.44.1", "1.44", 1, false},
		{"1.44", "1.44.1", -1, false},
		{"1", "1.44", -1, false},
		{"1.9", "1.44", -1, false}, // numeric, not lexicographic
		{"v1.44", "1.44", 0, true}, // non-numeric part -> error, like TS NaN check
		{"1.44", "abc", 0, true},
	}
	for _, tc := range cases {
		got, err := compareAPIVersions(tc.current, tc.required)
		if tc.wantErr {
			if err == nil {
				t.Errorf("compareAPIVersions(%q, %q): expected error, got nil", tc.current, tc.required)
			}
			continue
		}
		if err != nil {
			t.Errorf("compareAPIVersions(%q, %q): unexpected error: %v", tc.current, tc.required, err)
			continue
		}
		if got != tc.want {
			t.Errorf("compareAPIVersions(%q, %q) = %d, want %d", tc.current, tc.required, got, tc.want)
		}
	}
}

func TestDockerFilterURL(t *testing.T) {
	d := newDockerIntegration(&DockerConfig{ContainerName: "headscale"}, testLogger())
	path, err := d.containersURL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, "/v1.44/containers/json?") {
		t.Fatalf("unexpected path prefix: %q", path)
	}
	raw := strings.TrimPrefix(path, "/v1.44/containers/json?")
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	var filters map[string][]string
	if err := json.Unmarshal([]byte(q.Get("filters")), &filters); err != nil {
		t.Fatal(err)
	}
	if got := filters["name"]; len(got) != 1 || got[0] != "headscale" {
		t.Fatalf("unexpected name filter: %v", got)
	}

	d = newDockerIntegration(&DockerConfig{ContainerLabel: "a=b"}, testLogger())
	path, err = d.containersURL()
	if err != nil {
		t.Fatal(err)
	}
	raw = strings.TrimPrefix(path, "/v1.44/containers/json?")
	q, err = url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	filters = nil
	if err := json.Unmarshal([]byte(q.Get("filters")), &filters); err != nil {
		t.Fatal(err)
	}
	if got := filters["label"]; len(got) != 1 || got[0] != "a=b" {
		t.Fatalf("unexpected label filter: %v", got)
	}
}

// dockerTestServer spins up an HTTP server on a unix socket and returns the
// socket path plus a mux the test can register handlers on.
func dockerTestServer(t *testing.T) (string, *http.ServeMux) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sockPath, mux
}

func TestDockerIsAvailableUnixSocket(t *testing.T) {
	sockPath, mux := dockerTestServer(t)

	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ApiVersion":"1.44"}`))
	})
	mux.HandleFunc("/v1.44/containers/json", func(w http.ResponseWriter, r *http.Request) {
		var filters map[string][]string
		if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
			http.Error(w, "bad filters", http.StatusBadRequest)
			return
		}
		if got := filters["label"]; len(got) != 1 || got[0] != "me.tale.headplane.target=headscale" {
			http.Error(w, "unexpected filters", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"Id":"abc123","Names":["/headscale"]}]`))
	})

	d := newDockerIntegration(&DockerConfig{Socket: "unix://" + sockPath}, testLogger())
	if !d.IsAvailable() {
		t.Fatal("expected integration to be available")
	}
	if d.containerID != "abc123" {
		t.Fatalf("unexpected container ID: %q", d.containerID)
	}
}

func TestDockerIsAvailableVersionTooOld(t *testing.T) {
	sockPath, mux := dockerTestServer(t)
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ApiVersion":"1.43"}`))
	})

	d := newDockerIntegration(&DockerConfig{Socket: "unix://" + sockPath}, testLogger())
	if d.IsAvailable() {
		t.Fatal("expected integration to be unavailable with API 1.43")
	}
}

func TestDockerIsAvailableMissingSocket(t *testing.T) {
	d := newDockerIntegration(&DockerConfig{Socket: "unix:///nonexistent/docker.sock"}, testLogger())
	if d.IsAvailable() {
		t.Fatal("expected integration to be unavailable with missing socket")
	}
}

func TestDockerIsAvailableMultipleContainers(t *testing.T) {
	sockPath, mux := dockerTestServer(t)
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ApiVersion":"1.44"}`))
	})
	mux.HandleFunc("/v1.44/containers/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"Id":"a"},{"Id":"b"}]`))
	})

	d := newDockerIntegration(&DockerConfig{Socket: "unix://" + sockPath}, testLogger())
	if d.IsAvailable() {
		t.Fatal("expected integration to be unavailable with multiple containers")
	}
}

func TestDockerIsAvailableTCP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/version":
			_, _ = w.Write([]byte(`{"ApiVersion":"1.44"}`))
		case strings.HasPrefix(r.URL.Path, "/v1.44/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"tcp1","Names":["/headscale"]}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	d := newDockerIntegration(&DockerConfig{Socket: "tcp://" + u.Host}, testLogger())
	if !d.IsAvailable() {
		t.Fatal("expected integration to be available over TCP")
	}
	if d.containerID != "tcp1" {
		t.Fatalf("unexpected container ID: %q", d.containerID)
	}
}

func TestDockerRestartRetry(t *testing.T) {
	noSleep(t)
	sockPath, mux := dockerTestServer(t)
	var restarts atomic.Int32
	mux.HandleFunc("/v1.44/containers/abc123/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if n := restarts.Add(1); n < 3 {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	d := newDockerIntegration(&DockerConfig{Socket: "unix://" + sockPath}, testLogger())
	d.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}}
	d.baseURL = "http://localhost"
	d.containerID = "abc123"

	healthCalls := 0
	health := func() bool {
		healthCalls++
		return true
	}
	if err := d.OnConfigChange(health); err != nil {
		t.Fatalf("OnConfigChange returned error: %v", err)
	}
	if got := restarts.Load(); got != 3 {
		t.Fatalf("expected 3 restart attempts, got %d", got)
	}
	if healthCalls != 1 {
		t.Fatalf("expected 1 health check, got %d", healthCalls)
	}
}

func TestDockerRestartExhausted(t *testing.T) {
	noSleep(t)
	sockPath, mux := dockerTestServer(t)
	var restarts atomic.Int32
	mux.HandleFunc("/v1.44/containers/abc123/restart", func(w http.ResponseWriter, r *http.Request) {
		restarts.Add(1)
		http.Error(w, "still busy", http.StatusInternalServerError)
	})

	d := newDockerIntegration(&DockerConfig{Socket: "unix://" + sockPath}, testLogger())
	d.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}}
	d.baseURL = "http://localhost"
	d.containerID = "abc123"

	err := d.OnConfigChange(func() bool { return true })
	if err == nil {
		t.Fatal("expected error after exhausting restart retries")
	}
	// TS loop is `attempts <= maxAttempts` with maxAttempts=10 -> 11 attempts.
	if got := restarts.Load(); got != 11 {
		t.Fatalf("expected 11 restart attempts, got %d", got)
	}
}

func TestDockerHealthDeadlineMissed(t *testing.T) {
	noSleep(t)
	// Restart succeeds, health never reports healthy: the TS logs
	// "Missed restart deadline" and returns without an error.
	sockPath, mux := dockerTestServer(t)
	mux.HandleFunc("/v1.44/containers/abc123/restart", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	d := newDockerIntegration(&DockerConfig{Socket: "unix://" + sockPath}, testLogger())
	d.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}}
	d.baseURL = "http://localhost"
	d.containerID = "abc123"

	var healthCalls atomic.Int32
	if err := d.OnConfigChange(func() bool {
		healthCalls.Add(1)
		return false
	}); err != nil {
		t.Fatalf("expected nil error on missed deadline (mirrors TS), got %v", err)
	}
	// Immediate first check + 10 retries = 11 health calls.
	if got := healthCalls.Load(); got != 11 {
		t.Fatalf("expected 11 health checks, got %d", got)
	}
}

func TestDockerDefaults(t *testing.T) {
	d := newDockerIntegration(&DockerConfig{}, testLogger())
	if d.socket != defaultDockerSocket {
		t.Fatalf("unexpected default socket: %q", d.socket)
	}
	if d.containerLabel != defaultDockerContainerLabel {
		t.Fatalf("unexpected default label: %q", d.containerLabel)
	}
}

func TestMain(m *testing.M) {
	// Silence the package logger for tests.
	pkgLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
	os.Exit(m.Run())
}
