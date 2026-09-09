package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeWasm(t *testing.T, dir string) {
	t.Helper()
	for _, f := range []string{"hp_ssh.wasm", "hp_rdp.wasm", "wasm_exec.js"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The route must dispatch to the terminal handler (JSON), never the SPA
// shell or the generic v1 404.
func TestV1TerminalRouted(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	srv.clientDir = t.TempDir()
	for _, path := range []string{"/ssh/", "/rdp/", "/ssh/nope"} {
		rec := v1Get(t, srv, cookie, path)
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
			t.Fatalf("%s: content-type = %q, want json", path, ct)
		}
		if rec.Code == http.StatusOK {
			t.Fatalf("%s: unexpected 200", path)
		}
	}
}

// WASM assets absent from the client bundle -> 405 with the page's error
// body; the SSH body carries an `anchor`, the RDP body does not.
func TestV1TerminalWasmMissing(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	srv.clientDir = t.TempDir() // empty

	cases := []struct {
		path       string
		wantAnchor bool
	}{
		{"/ssh/node1", true},
		{"/rdp/node1", false},
	}
	for _, c := range cases {
		rec := v1Get(t, srv, cookie, c.path)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: code = %d (%s), want 405", c.path, rec.Code, rec.Body.String())
		}
		b := decodeBody(t, rec)
		if b["title"] == nil || b["message"] == nil {
			t.Fatalf("%s: body = %v", c.path, b)
		}
		if c.wantAnchor && b["anchor"] == nil {
			t.Fatalf("%s: SSH error must carry anchor: %v", c.path, b)
		}
		if !c.wantAnchor && b["anchor"] != nil {
			t.Fatalf("%s: RDP error must not carry anchor: %v", c.path, b)
		}
	}
}

// WASM present but the agent integration disabled -> 400 agent-required.
func TestV1TerminalAgentRequired(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	dir := t.TempDir()
	writeFakeWasm(t, dir)
	srv.clientDir = dir

	rec := v1Get(t, srv, cookie, "/ssh/node1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	if decodeBody(t, rec)["anchor"] != "#agent-required" {
		t.Fatalf("body = %s", rec.Body.String())
	}

	rec = v1Get(t, srv, cookie, "/rdp/node1")
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["title"] != "Agent Required" {
		t.Fatalf("rdp: code = %d body = %s", rec.Code, rec.Body.String())
	}
}

// Unauthenticated -> 401 (before any WASM/agent work).
func TestV1TerminalUnauthorized(t *testing.T) {
	srv, _, _ := testV1Server(t, defaultStub())
	srv.clientDir = t.TempDir()
	rec := v1Get(t, srv, "", "/ssh/node1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}
