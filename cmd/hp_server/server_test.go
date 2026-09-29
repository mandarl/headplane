package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tale/headplane/internal/serverconfig"
)

// testServer builds a Server over a temp client dir with a few fixture files.
func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.html"), "<html>spa</html>")
	writeFile(t, filepath.Join(dir, "assets", "app-abc123.js"), "console.log(1)")
	writeFile(t, filepath.Join(dir, "favicon.ico"), "icon")
	cfg := &serverconfig.Config{}
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = 0
	return newServer(cfg, "/admin", dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBasenameRedirect(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/" {
		t.Fatalf("Location = %q, want /admin/", loc)
	}

	// Query string is preserved.
	req = httptest.NewRequest(http.MethodGet, "/admin?next=/machines", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); loc != "/admin/?next=/machines" {
		t.Fatalf("Location = %q, want query preserved", loc)
	}
}

func TestHealthz(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"status":"OK"}` {
		t.Fatalf("body = %q", rec.Body.String())
	}

	srv.healthCheck = func() bool { return false }
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unhealthy status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ERROR"`) {
		t.Fatalf("unhealthy body = %q", rec.Body.String())
	}
}

func TestStaticCacheHeaders(t *testing.T) {
	srv := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/admin/assets/app-abc123.js", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("asset status = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("asset cache-control = %q", cc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("asset content-type = %q", ct)
	}
	if rec.Body.String() != "console.log(1)" {
		t.Fatalf("asset body = %q", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/favicon.ico", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Fatalf("favicon cache-control = %q", cc)
	}
}

func TestHeadRequest(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest(http.MethodHead, "/admin/assets/app-abc123.js", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD body should be empty, got %d bytes", rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD should carry Content-Length")
	}
}

func TestTraversalRejected(t *testing.T) {
	srv := testServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Raw socket-style request: Go's client sends the escaped path as-is.
	resp, err := http.Get(ts.URL + "/admin/%2e%2e/%2e%2e/package.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("traversal status = %d, want 404", resp.StatusCode)
	}
	if strings.Contains(string(body), "headplane") && strings.Contains(string(body), `"name"`) {
		t.Fatalf("traversal leaked file content: %q", body)
	}

	// Plain ../ in a decoded path also 404s (and never serves index.html).
	resp2, err := http.Get(ts.URL + "/admin/..%2f..%2fetc%2fpasswd")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("encoded traversal status = %d, want 404", resp2.StatusCode)
	}
}

func TestSPAFallback(t *testing.T) {
	srv := testServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/admin/machines/abc")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fallback status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "<html>spa</html>" {
		t.Fatalf("fallback body = %q", body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("fallback cache-control = %q", cc)
	}

	// Outside the basename: 404.
	resp2, err := http.Get(ts.URL + "/other")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("outside basename status = %d, want 404", resp2.StatusCode)
	}
}

func TestMethodNotAllowedPaths(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/assets/app-abc123.js", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST status = %d, want 404", rec.Code)
	}
}

func TestMissingIndexIs500(t *testing.T) {
	dir := t.TempDir() // no index.html
	cfg := &serverconfig.Config{}
	srv := newServer(cfg, "/admin", dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing index status = %d, want 500", rec.Code)
	}
}
