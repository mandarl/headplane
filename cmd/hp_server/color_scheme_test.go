package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHandleColorScheme(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())

	post := func(form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/color-scheme", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// dark -> Set-Cookie color_scheme = urlencode(base64(json)) + 302 redirect
	rec := post(url.Values{"colorScheme": {"dark"}, "returnTo": {"/machines"}})
	if rec.Code != http.StatusFound {
		t.Fatalf("dark: status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/machines" {
		t.Fatalf("dark: Location = %q", loc)
	}
	sc := rec.Header().Get("Set-Cookie")
	if !strings.HasPrefix(sc, "color_scheme=") || !strings.Contains(sc, "SameSite=Lax") {
		t.Fatalf("dark: Set-Cookie = %q", sc)
	}
	val := strings.TrimPrefix(strings.SplitN(sc, ";", 2)[0], "color_scheme=")
	dec, err := url.QueryUnescape(val)
	if err != nil {
		t.Fatalf("cookie value not url-encoded: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(dec)
	if err != nil {
		t.Fatalf("cookie value not base64: %v (%q)", err, dec)
	}
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil || payload["colorScheme"] != "dark" {
		t.Fatalf("cookie payload = %s (%v)", raw, err)
	}

	// system -> cookie cleared
	rec = post(url.Values{"colorScheme": {"system"}})
	if rec.Code != http.StatusFound {
		t.Fatalf("system: status = %d", rec.Code)
	}
	if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=0") {
		t.Fatalf("system: Set-Cookie = %q, want Max-Age=0", sc)
	}

	// invalid value -> 400, no cookie
	rec = post(url.Values{"colorScheme": {"neon"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad: status = %d, want 400", rec.Code)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("bad: unexpected Set-Cookie %q", rec.Header().Get("Set-Cookie"))
	}

	// returnTo sanitization: protocol-relative -> "/"
	rec = post(url.Values{"colorScheme": {"light"}, "returnTo": {"//evil.example"}})
	if loc := rec.Header().Get("Location"); loc != "/admin/" {
		t.Fatalf("returnTo //evil: Location = %q, want /admin/", loc)
	}

	// GET is rejected
	req := httptest.NewRequest(http.MethodGet, "/admin/api/color-scheme", nil)
	req.Header.Set("Cookie", cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status = %d, want 405", rec.Code)
	}

	// multipart/form-data body (what header.tsx's `fetch` with a FormData
	// body actually sends) must work, not just urlencoded.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("colorScheme", "light")
	_ = mw.WriteField("returnTo", "/dns")
	mw.Close()
	req = httptest.NewRequest(http.MethodPost, "/admin/api/color-scheme", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Cookie", cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("multipart: status = %d (%s), want 302", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/dns" {
		t.Fatalf("multipart: Location = %q", loc)
	}
	if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, "color_scheme=") || strings.Contains(sc, "color_scheme=;") {
		t.Fatalf("multipart: Set-Cookie = %q", sc)
	}
}

// TestUtilityRoutesRegistered guards against the "route exists in
// app/routes.ts but no Go handler" gap (customRouting, ssh/rdp, and
// color-scheme were all found in production). Every non-/api/v1 utility
// route must be dispatched to a handler — never fall through to the
// generic notFound() or the SPA shell.
func TestUtilityRoutesRegistered(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())

	cases := []struct{ method, path string }{
		{http.MethodGet, "/admin/healthz"},
		{http.MethodPost, "/admin/login"},
		{http.MethodPost, "/admin/logout"},
		{http.MethodGet, "/admin/oidc/start"},
		{http.MethodGet, "/admin/oidc/callback"},
		{http.MethodGet, "/admin/events/live"},
		{http.MethodPost, "/admin/api/rdp-gateway"},
		{http.MethodGet, "/admin/api/info"},
		{http.MethodPost, "/admin/api/color-scheme"},
		{http.MethodGet, "/admin/ssh/node1"},
		{http.MethodGet, "/admin/rdp/node1"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(""))
		req.Header.Set("Cookie", cookie)
		if c.method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), `"error":"Not found"`) {
			t.Errorf("%s %s fell through to notFound() — no handler registered", c.method, c.path)
		}
		if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s %s resolved to the SPA shell (%s) — no handler registered", c.method, c.path, ct)
		}
	}
}
