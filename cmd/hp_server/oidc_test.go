package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tale/headplane/internal/auth"
	"github.com/tale/headplane/internal/serverconfig"
	_ "modernc.org/sqlite"
)

// testOIDCProvider is a mock OIDC provider + JWT signer for the HTTP-layer
// tests: discovery, JWKS, token, userinfo and end-session endpoints.
type testOIDCProvider struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	issuer string
	// nonce is echoed into issued ID tokens, like a real provider echoes
	// the authorization-request nonce.
	nonce string
}

func newTestOIDCProvider(t *testing.T) *testOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	op := &testOIDCProvider{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 op.issuer,
			"authorization_endpoint": op.issuer + "/authorize",
			"token_endpoint":         op.issuer + "/token",
			"jwks_uri":               op.issuer + "/jwks",
			"userinfo_endpoint":      op.issuer + "/userinfo",
			"end_session_endpoint":   op.issuer + "/end",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(op.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(op.key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT","kid":"k1"}`))
		payloadJSON, _ := json.Marshal(map[string]any{
			"iss": op.issuer, "aud": "test-client", "sub": "subject-123",
			"name": "SSO Person", "email": "sso@example.com", "nonce": op.nonce,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		})
		payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
		h := sha256.New()
		h.Write([]byte(header + "." + payload))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, op.key, crypto.SHA256, h.Sum(nil))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer",
			"id_token": header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(sig),
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	op.server = httptest.NewServer(mux)
	op.issuer = op.server.URL
	t.Cleanup(op.server.Close)
	return op
}

// testOIDCServer builds a Server with OIDC enabled against the mock
// provider. useEndSession toggles oidc.use_end_session; headscaleUsers, when
// non-nil, is served from GET /api/v1/user of a mock Headscale.
func testOIDCServer(t *testing.T, op *testOIDCProvider, useEndSession bool, headscaleUsers []map[string]any) *Server {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/hp_persist.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(authTestSchema); err != nil {
		t.Fatal(err)
	}

	hsURL := ""
	if headscaleUsers != nil {
		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/user" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"users": headscaleUsers})
		}))
		t.Cleanup(hs.Close)
		hsURL = hs.URL
	}

	cfg := &serverconfig.Config{}
	cfg.Server.BaseURL = "http://localhost:3000"
	cfg.Server.CookieMaxAge = 3600
	cfg.Headscale.URL = hsURL
	cfg.Headscale.APIKey = "test-hs-key"
	cfg.OIDC = &serverconfig.OIDCConfig{
		Issuer:        op.issuer,
		ClientID:      "test-client",
		ClientSecret:  "test-secret",
		UseEndSession: useEndSession,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newServer(cfg, "/admin", t.TempDir(), logger)
	srv.authSvc = auth.NewService(db, "0123456789abcdef0123456789abcdef", "test-hs-key", auth.CookieOptions{
		Name: "_hp_auth", Path: "/admin", MaxAge: 3600,
	})
	svc, reason := newOidcService(cfg, "/admin", logger)
	if svc == nil {
		t.Fatalf("OIDC unexpectedly disabled: %s", reason)
	}
	srv.oidcSvc = svc
	srv.oidcDisabledReason = reason
	return srv
}

// oidcStart performs the start step and returns the response plus the parsed
// state cookie values.
func oidcStart(t *testing.T, srv *Server, cookieHeader string) (*httptest.ResponseRecorder, *oidcState) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/oidc/start", nil)
	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var st *oidcState
	for _, c := range rec.Result().Cookies() {
		_ = c
	}
	// Parse the __oidc_state Set-Cookie via a synthetic request.
	for _, setCookie := range rec.Result().Header.Values("Set-Cookie") {
		if strings.HasPrefix(setCookie, "__oidc_state=") {
			r2 := httptest.NewRequest(http.MethodGet, "/", nil)
			r2.Header.Set("Cookie", strings.SplitN(setCookie, ";", 2)[0])
			var ok bool
			st, ok = parseOidcStateCookie(r2)
			if !ok {
				t.Fatal("start did not set a parseable __oidc_state cookie")
			}
		}
	}
	return rec, st
}

func TestOidcStartRedirect(t *testing.T) {
	op := newTestOIDCProvider(t)
	srv := testOIDCServer(t, op, false, nil)

	rec, st := oidcStart(t, srv, "")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != op.issuer+"/authorize" {
		t.Errorf("Location = %s", loc)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"response_type": "code", "client_id": "test-client",
		"redirect_uri": "http://localhost:3000/admin/oidc/callback",
		"scope":        "openid email profile",
	} {
		if q.Get(k) != want {
			t.Errorf("param %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if st == nil || st.State == "" || q.Get("state") != st.State || q.Get("nonce") != st.Nonce {
		t.Errorf("state cookie does not match auth URL params")
	}

	// Cookie attributes mirror the TS createCookie options.
	var found bool
	for _, setCookie := range rec.Header().Values("Set-Cookie") {
		if !strings.HasPrefix(setCookie, "__oidc_state=") {
			continue
		}
		found = true
		for _, attr := range []string{"Path=/admin/oidc/callback", "Max-Age=1800", "HttpOnly", "SameSite=Lax"} {
			if !strings.Contains(setCookie, attr) {
				t.Errorf("Set-Cookie missing %q: %s", attr, setCookie)
			}
		}
	}
	if !found {
		t.Error("no __oidc_state Set-Cookie")
	}
}

func TestOidcStartAuthenticated(t *testing.T) {
	op := newTestOIDCProvider(t)
	srv := testOIDCServer(t, op, false, nil)

	// Mint a session directly and use its cookie.
	userID, err := srv.authSvc.FindOrCreateUser("sub-1", strPtr("A"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	setCookie, err := srv.authSvc.CreateOidcSession(userID, auth.CookieProfile{Name: "A"}, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/oidc/start", nil)
	req.Header.Set("Cookie", strings.SplitN(setCookie, ";", 2)[0])
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/" {
		t.Errorf("status = %d, Location = %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestOidcStartDisabled(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/hp_persist.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(authTestSchema); err != nil {
		t.Fatal(err)
	}
	cfg := &serverconfig.Config{}
	srv := newServer(cfg, "/admin", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.authSvc = auth.NewService(db, "0123456789abcdef0123456789abcdef", "", auth.CookieOptions{Name: "_hp_auth"})
	svc, reason := newOidcService(cfg, "/admin", slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.oidcSvc = svc
	srv.oidcDisabledReason = reason

	req := httptest.NewRequest(http.MethodGet, "/admin/oidc/start", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "OIDC is unavailable: OIDC is not configured") {
		t.Errorf("body = %q", body)
	}
}

func TestOidcCallbackErrors(t *testing.T) {
	op := newTestOIDCProvider(t)
	srv := testOIDCServer(t, op, false, nil)

	get := func(target, cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// No query string.
	if rec := get("/admin/oidc/callback", ""); rec.Header().Get("Location") != "/admin/login?s=error_no_query" {
		t.Errorf("no query: Location = %q", rec.Header().Get("Location"))
	}
	// No state cookie.
	if rec := get("/admin/oidc/callback?code=x&state=y", ""); rec.Header().Get("Location") != "/admin/login?s=error_no_session" {
		t.Errorf("no cookie: Location = %q", rec.Header().Get("Location"))
	}
	// Cookie missing required fields.
	bad := "__oidc_state=" + auth.EncodeURIComponent(base64.StdEncoding.EncodeToString([]byte(`{"state":"s"}`)))
	if rec := get("/admin/oidc/callback?code=x&state=s", bad); rec.Header().Get("Location") != "/admin/login?s=error_invalid_session" {
		t.Errorf("bad cookie: Location = %q", rec.Header().Get("Location"))
	}
	// State mismatch → error_auth_failed.
	rec, st := oidcStart(t, srv, "")
	_ = st
	cookieHdr := ""
	for _, sc := range rec.Header().Values("Set-Cookie") {
		if strings.HasPrefix(sc, "__oidc_state=") {
			cookieHdr = strings.SplitN(sc, ";", 2)[0]
		}
	}
	if rec := get("/admin/oidc/callback?code=x&state=wrong", cookieHdr); rec.Header().Get("Location") != "/admin/login?s=error_auth_failed" {
		t.Errorf("state mismatch: Location = %q", rec.Header().Get("Location"))
	}
}

// oidcLogin performs a full start→callback round trip and returns the
// _hp_auth cookie value.
func oidcLogin(t *testing.T, srv *Server, op *testOIDCProvider) string {
	t.Helper()
	rec, st := oidcStart(t, srv, "")
	if st == nil {
		t.Fatal("no state from start")
	}
	op.nonce = st.Nonce
	var cookieHdr string
	for _, sc := range rec.Header().Values("Set-Cookie") {
		if strings.HasPrefix(sc, "__oidc_state=") {
			cookieHdr = strings.SplitN(sc, ";", 2)[0]
		}
	}
	req := httptest.NewRequest(http.MethodGet,
		"/admin/oidc/callback?code=authcode&state="+url.QueryEscape(st.State), nil)
	req.Header.Set("Cookie", cookieHdr)
	cb := httptest.NewRecorder()
	srv.ServeHTTP(cb, req)
	if cb.Code != http.StatusFound {
		t.Fatalf("callback status = %d, Location = %q", cb.Code, cb.Header().Get("Location"))
	}
	if loc := cb.Header().Get("Location"); loc != "/admin/" {
		t.Fatalf("callback Location = %q, want /", loc)
	}
	for _, sc := range cb.Header().Values("Set-Cookie") {
		if strings.HasPrefix(sc, "_hp_auth=") {
			return strings.SplitN(sc, ";", 2)[0]
		}
	}
	t.Fatal("callback did not set _hp_auth")
	return ""
}

func strPtr(s string) *string { return &s }

func TestOidcCallbackSuccess(t *testing.T) {
	op := newTestOIDCProvider(t)
	hsUsers := []map[string]any{
		{"id": "42", "email": "sso@example.com", "provider": "oidc", "providerId": "https://idp.example.com/subject-123"},
	}
	srv := testOIDCServer(t, op, false, hsUsers)

	authCookie := oidcLogin(t, srv, op)

	// The session cookie validates.
	req := httptest.NewRequest(http.MethodGet, "/admin/logout", nil)
	req.Header.Set("Cookie", authCookie)
	principal, err := srv.authSvc.Require(req)
	if err != nil {
		t.Fatalf("session does not validate: %v", err)
	}
	if principal.Kind != "oidc" || principal.Subject != "subject-123" {
		t.Errorf("principal = %+v", principal)
	}
	if principal.IDToken != nil {
		t.Errorf("id_token persisted without use_end_session")
	}

	// First user is promoted to owner, and the Headscale user was linked.
	db := srv.authSvc.DB()
	var role, hsID string
	var caps int64
	if err := db.QueryRow(`SELECT role, headscale_user_id, caps FROM users WHERE sub = 'subject-123'`).Scan(&role, &hsID, &caps); err != nil {
		t.Fatalf("user row: %v", err)
	}
	if role != "owner" {
		t.Errorf("role = %q, want owner (first user)", role)
	}
	if hsID != "42" {
		t.Errorf("headscale_user_id = %q, want 42", hsID)
	}
	_ = caps
}

func TestOidcLogoutEndSession(t *testing.T) {
	op := newTestOIDCProvider(t)
	srv := testOIDCServer(t, op, true, nil)

	authCookie := oidcLogin(t, srv, op)

	req := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	req.Header.Set("Cookie", authCookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Scheme+"://"+loc.Host+loc.Path != op.issuer+"/end" {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
	q := loc.Query()
	if q.Get("id_token_hint") == "" {
		t.Errorf("missing id_token_hint in %q", rec.Header().Get("Location"))
	}
	if q.Get("client_id") != "test-client" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("post_logout_redirect_uri") != "http://localhost:3000/admin/login?s=logout" {
		t.Errorf("post_logout_redirect_uri = %q", q.Get("post_logout_redirect_uri"))
	}

	// The local session is destroyed and the cookie cleared regardless.
	var count int
	if err := srv.authSvc.DB().QueryRow(`SELECT count(*) FROM auth_sessions`).Scan(&count); err != nil || count != 0 {
		t.Errorf("sessions remaining = %d, err = %v", count, err)
	}
	cleared := false
	for _, sc := range rec.Header().Values("Set-Cookie") {
		if strings.HasPrefix(sc, "_hp_auth=") {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not clear _hp_auth")
	}
}

func TestOidcLogoutWithoutEndSession(t *testing.T) {
	op := newTestOIDCProvider(t)
	srv := testOIDCServer(t, op, false, nil)

	authCookie := oidcLogin(t, srv, op)

	req := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	req.Header.Set("Cookie", authCookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/login" {
		t.Errorf("Location = %q, want /admin/login", loc)
	}
}

// TestOidcStateCookieNodeInterop pins the rolling-deploy interop: a
// __oidc_state cookie serialized by the TypeScript implementation (value
// generated by Node's Buffer.toString("base64") + encodeURIComponent, the
// same pipeline React Router's createCookie uses) must parse in Go, and the
// Go serializer must produce byte-identical output for the same object.
func TestOidcStateCookieNodeInterop(t *testing.T) {
	// Produced by: encodeURIComponent(Buffer.from(JSON.stringify({
	//   state: "st_abc123", nonce: "n_xyz", verifier: "v_0123456789",
	//   redirect_uri: "http://localhost:3000/admin/oidc/callback",
	// })).toString("base64"))
	const nodeValue = "eyJzdGF0ZSI6InN0X2FiYzEyMyIsIm5vbmNlIjoibl94eXoiLCJ2ZXJpZmllciI6InZfMDEyMzQ1Njc4OSIsInJlZGlyZWN0X3VyaSI6Imh0dHA6Ly9sb2NhbGhvc3Q6MzAwMC9hZG1pbi9vaWRjL2NhbGxiYWNrIn0%3D"

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", "__oidc_state="+nodeValue)
	st, ok := parseOidcStateCookie(req)
	if !ok {
		t.Fatal("Go failed to parse the Node-serialized cookie")
	}
	if st.State != "st_abc123" || st.Nonce != "n_xyz" || st.Verifier != "v_0123456789" ||
		st.RedirectURI != "http://localhost:3000/admin/oidc/callback" {
		t.Errorf("parsed = %+v", st)
	}
	if got := encodeOidcState(*st); got != nodeValue {
		t.Errorf("Go serialization differs from Node:\n got %q\nwant %q", got, nodeValue)
	}
}
