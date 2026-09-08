package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tale/headplane/internal/auth"
	"github.com/tale/headplane/internal/serverconfig"
	_ "modernc.org/sqlite"
)

const authTestSchema = `
CREATE TABLE auth_sessions (
	id text PRIMARY KEY NOT NULL,
	kind text NOT NULL,
	user_id text,
	api_key_hash text,
	api_key_display text,
	oidc_id_token text,
	expires_at integer NOT NULL,
	created_at integer
);
CREATE TABLE users (
	id text PRIMARY KEY NOT NULL,
	sub text NOT NULL UNIQUE,
	name text,
	email text,
	picture text,
	role text NOT NULL DEFAULT 'member',
	headscale_user_id text UNIQUE,
	created_at integer,
	updated_at integer,
	last_login_at integer,
	caps integer NOT NULL DEFAULT 0
);
CREATE TABLE host_info (
	host_id text PRIMARY KEY NOT NULL,
	payload text NOT NULL,
	updated_at integer
);
CREATE TABLE service_description_overrides (
	id text PRIMARY KEY NOT NULL,
	host_id text NOT NULL,
	proto text NOT NULL,
	port integer NOT NULL,
	description text NOT NULL,
	updated_by text,
	updated_at integer
);`

// mockHeadscale serves GET /api/v1/apikey like Headscale: 200 with the
// caller's key list when the bearer token is a known key, 401 otherwise.
func mockHeadscale(t *testing.T, keysByBearer map[string][]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apikey" {
			http.NotFound(w, r)
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		keys, ok := keysByBearer[bearer]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"apiKeys": keys})
	}))
}

// testAuthServer builds a Server whose auth service is backed by a temp DB
// and whose Headscale URL points at the mock.
func testAuthServer(t *testing.T, headscaleURL string) *Server {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/hp_persist.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(authTestSchema); err != nil {
		t.Fatal(err)
	}
	cfg := &serverconfig.Config{}
	cfg.Headscale.URL = headscaleURL
	srv := newServer(cfg, "/admin", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.authSvc = auth.NewService(db, "0123456789abcdef0123456789abcdef", "", auth.CookieOptions{
		Name: "_hp_auth", Path: "/admin", MaxAge: 3600,
	})
	return srv
}

func postLogin(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestLoginSuccess(t *testing.T) {
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	hs := mockHeadscale(t, map[string][]map[string]any{
		"test-prefix-secret": {{"prefix": "test-prefix", "expiration": expiry}},
	})
	defer hs.Close()
	srv := testAuthServer(t, hs.URL)

	rec := postLogin(t, srv, "api_key=test-prefix-secret")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/machines" {
		t.Fatalf("Location = %q, want /admin/machines", loc)
	}
	setCookie := rec.Header().Get("Set-Cookie")
	// Max-Age is floor(seconds until the Headscale key expiry): 3599 or 3600
	// depending on sub-second truncation, exactly like the TS Math.floor.
	if !strings.HasPrefix(setCookie, "_hp_auth=") ||
		(!strings.Contains(setCookie, "Max-Age=3600") && !strings.Contains(setCookie, "Max-Age=3599")) {
		t.Fatalf("bad Set-Cookie: %q", setCookie)
	}

	// The issued session must resolve through the service.
	cookieVal := strings.SplitN(strings.TrimPrefix(setCookie, "_hp_auth="), ";", 2)[0]
	req := httptest.NewRequest(http.MethodGet, "/admin/machines", nil)
	req.Header.Set("Cookie", "_hp_auth="+cookieVal)
	p, err := srv.authSvc.Require(req)
	if err != nil {
		t.Fatalf("issued session does not resolve: %v", err)
	}
	if p.Kind != "api_key" || p.APIKey != "test-prefix-secret" {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

func TestLoginFailures(t *testing.T) {
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	hs := mockHeadscale(t, map[string][]map[string]any{
		"good-key":     {{"prefix": "good", "expiration": expiry}},
		"old-key":      {{"prefix": "old", "expiration": past}},
		"stranger-key": {{"prefix": "zzz", "expiration": expiry}},
	})
	defer hs.Close()
	srv := testAuthServer(t, hs.URL)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing key", "", "Missing API key. Please enter your API key."},
		{"empty key", "api_key=", "API key cannot be empty. Please enter a valid API key."},
		{"invalid key", "api_key=nope", "API key is invalid (it may be incorrect or expired)"},
		{"unknown key", "api_key=stranger-key", "API key was not found in the Headscale database"},
		{"expired key", "api_key=old-key", "API key has expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postLogin(t, srv, tc.body)
			// Failures stay HTTP 200 with a JSON body (React Router would
			// turn 4xx into an error page for document requests).
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var data map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
				t.Fatalf("bad JSON: %v (%q)", err, rec.Body.String())
			}
			if data["success"] != false || data["message"] != tc.want {
				t.Fatalf("got %v, want success=false message=%q", data, tc.want)
			}
		})
	}
}

func TestLogout(t *testing.T) {
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	hs := mockHeadscale(t, map[string][]map[string]any{
		"k": {{"prefix": "k", "expiration": expiry}},
	})
	defer hs.Close()
	srv := testAuthServer(t, hs.URL)

	// Log in to get a session cookie.
	rec := postLogin(t, srv, "api_key=k")
	cookieVal := strings.SplitN(strings.TrimPrefix(rec.Header().Get("Set-Cookie"), "_hp_auth="), ";", 2)[0]

	// POST /admin/logout destroys the session and clears the cookie.
	req := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	req.Header.Set("Cookie", "_hp_auth="+cookieVal)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/login" {
		t.Fatalf("Location = %q, want /admin/login", loc)
	}
	clear := rec.Header().Get("Set-Cookie")
	if !strings.HasPrefix(clear, "_hp_auth=;") || !strings.Contains(clear, "Expires=Thu, 01 Jan 1970") {
		t.Fatalf("bad clearing Set-Cookie: %q", clear)
	}

	// The session row is gone.
	var n int
	if err := srv.authSvc.DB().QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("session row not deleted by logout")
	}

	// The old cookie no longer resolves.
	req = httptest.NewRequest(http.MethodGet, "/admin/machines", nil)
	req.Header.Set("Cookie", "_hp_auth="+cookieVal)
	if _, err := srv.authSvc.Require(req); err == nil {
		t.Fatal("logged-out session still resolves")
	}
}

func TestLogoutUnauthenticated(t *testing.T) {
	srv := testAuthServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/login" {
		t.Fatalf("Location = %q, want /admin/login", loc)
	}
	// Mirrors the TS action: no session, no Set-Cookie.
	if sc := rec.Header().Get("Set-Cookie"); sc != "" {
		t.Fatalf("unexpected Set-Cookie: %q", sc)
	}
}

func TestLogoutGet(t *testing.T) {
	srv := testAuthServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/admin/logout", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/machines" {
		t.Fatalf("Location = %q, want /admin/machines", loc)
	}
}
