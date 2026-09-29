// mock_op_test.go builds a mock OIDC provider (httptest) against which the
// Go port's exact TS-compatible behaviors are verified.
package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockOP is a scriptable OIDC provider for tests.
type mockOP struct {
	server     *httptest.Server
	key        *rsa.PrivateKey
	kid        string
	issuer     string
	mu         sync.Mutex
	tokenCalls []tokenCall

	// tokenBehavior controls the token endpoint:
	// "ok" → always 200; "post-fail-then-basic-ok" → first post-auth call
	// fails with invalid_client, basic-auth calls succeed.
	tokenBehavior string
	seenMethods   []string

	userinfo map[string]any
	// signHook, when set, builds the ID token claims.
	signHook func() map[string]any
	// nonce, when set, is echoed into the ID token like a real provider
	// echoes the auth-request nonce (unless the hook sets one explicitly).
	nonce string
}

type tokenCall struct {
	method string // "post" or "basic"
}

func base64URLBigInt(n *big.Int) string {
	return base64.RawURLEncoding.EncodeToString(n.Bytes())
}

func newMockOP(t *testing.T, tokenBehavior string) *mockOP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	op := &mockOP{key: key, kid: "test-key", tokenBehavior: tokenBehavior}
	op.userinfo = map[string]any{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", op.discovery)
	mux.HandleFunc("/jwks", op.jwks)
	mux.HandleFunc("/token", op.token)
	mux.HandleFunc("/userinfo", op.userinfoHandler)
	mux.HandleFunc("/end", op.endSession)
	op.server = httptest.NewServer(mux)
	op.issuer = op.server.URL
	return op
}

func (op *mockOP) discovery(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{
		"issuer":                 op.issuer,
		"authorization_endpoint": op.server.URL + "/authorize",
		"token_endpoint":         op.server.URL + "/token",
		"jwks_uri":               op.server.URL + "/jwks",
		"userinfo_endpoint":      op.server.URL + "/userinfo",
		"end_session_endpoint":   op.server.URL + "/end",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

func (op *mockOP) jwks(w http.ResponseWriter, r *http.Request) {
	jwk := map[string]any{
		"kty": "RSA",
		"kid": op.kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64URLBigInt(op.key.N),
		"e":   base64URLBigInt(big.NewInt(int64(op.key.E))),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwk}})
}

func (op *mockOP) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	method := "post"
	if r.Header.Get("Authorization") != "" {
		method = "basic"
	}
	op.mu.Lock()
	op.seenMethods = append(op.seenMethods, method)
	op.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	if op.tokenBehavior == "post-fail-then-basic-ok" && method == "post" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_client",
			"error_description": "client auth failed",
		})
		return
	}

	claims := map[string]any{
		"iss":   op.issuer,
		"aud":   "headplane-test",
		"sub":   "mock-user-1",
		"name":  "Mock User",
		"email": "Mock@Example.COM",
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
	if op.signHook != nil {
		claims = op.signHook()
	}
	if op.nonce != "" {
		if _, ok := claims["nonce"]; !ok {
			claims["nonce"] = op.nonce
		}
	}
	idToken := signJWT(op.key, op.kid, claims)
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": "mock-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

func (op *mockOP) userinfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(op.userinfo)
}

func (op *mockOP) endSession(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func signJWT(key *rsa.PrivateKey, kid string, claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT","kid":"` + kid + `"}`))
	payloadJSON, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	h := sha256.New()
	h.Write([]byte(header + "." + payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h.Sum(nil))
	if err != nil {
		panic(err)
	}
	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func testCfg(op *mockOP) Config {
	return Config{
		Issuer:       op.issuer,
		ClientID:     "headplane-test",
		ClientSecret: "secret",
		BaseURL:      "http://localhost:3000",
		Basename:     "/admin",
		Scope:        "openid email profile",
	}
}

func TestStartFlowParams(t *testing.T) {
	op := newMockOP(t, "ok")
	defer op.server.Close()

	svc := NewService(testCfg(op), nil)
	authURL, flow, serr := svc.StartFlow(t.Context())
	op.nonce = flow.Nonce
	if serr != nil {
		t.Fatalf("StartFlow: %v", serr)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != op.server.URL+"/authorize" {
		t.Errorf("auth endpoint = %s", authURL)
	}
	q := u.Query()
	checks := map[string]string{
		"response_type": "code",
		"client_id":     "headplane-test",
		"redirect_uri":  "http://localhost:3000/admin/oidc/callback",
		"scope":         "openid email profile",
	}
	for k, want := range checks {
		if q.Get(k) != want {
			t.Errorf("param %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if q.Get("state") == "" || q.Get("nonce") == "" {
		t.Errorf("missing state/nonce")
	}
	if q.Get("state") != flow.State || q.Get("nonce") != flow.Nonce {
		t.Errorf("flow state does not match URL params")
	}
	if len(flow.State) != 43 { // 32 bytes base64url
		t.Errorf("state length = %d, want 43", len(flow.State))
	}
	if q.Get("code_challenge") != "" {
		t.Errorf("code_challenge present without use_pkce")
	}

	// With PKCE enabled, S256 challenge must be present.
	cfg := testCfg(op)
	cfg.UsePKCE = true
	svc2 := NewService(cfg, nil)
	authURL2, flow2, serr := svc2.StartFlow(t.Context())
	op.nonce = flow2.Nonce
	if serr != nil {
		t.Fatalf("StartFlow PKCE: %v", serr)
	}
	q2, _ := url.ParseQuery(strings.SplitN(authURL2, "?", 2)[1])
	if q2.Get("code_challenge_method") != "S256" {
		t.Errorf("challenge method = %q", q2.Get("code_challenge_method"))
	}
	sum := sha256.Sum256([]byte(flow2.CodeVerifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if q2.Get("code_challenge") != want {
		t.Errorf("challenge mismatch")
	}

	// extra_params applied after, able to override.
	cfg3 := testCfg(op)
	cfg3.ExtraParams = map[string]string{"scope": "openid custom", "audience": "api"}
	svc3 := NewService(cfg3, nil)
	authURL3, _, oerr := svc3.StartFlow(t.Context())
	if oerr != nil {
		t.Fatalf("StartFlow extra: %v", oerr)
	}
	q3, _ := url.ParseQuery(strings.SplitN(authURL3, "?", 2)[1])
	if q3.Get("scope") != "openid custom" || q3.Get("audience") != "api" {
		t.Errorf("extra params not applied: %v", q3)
	}
}

func TestCallbackFullFlow(t *testing.T) {
	op := newMockOP(t, "ok")
	defer op.server.Close()

	svc := NewService(testCfg(op), nil)
	_, flow, serr := svc.StartFlow(t.Context())
	op.nonce = flow.Nonce
	if serr != nil {
		t.Fatalf("StartFlow: %v", serr)
	}
	ident, oerr := svc.HandleCallback(t.Context(), url.Values{
		"code":  {"auth-code-1"},
		"state": {flow.State},
	}, flow)
	if oerr != nil {
		t.Fatalf("HandleCallback: %v", oerr)
	}
	if ident.Subject != "mock-user-1" || ident.Name != "Mock User" || ident.Username != "Mock" {
		t.Errorf("identity = %+v", ident)
	}
	if ident.Email != "Mock@Example.COM" {
		t.Errorf("email = %q", ident.Email)
	}
	if ident.IDToken == "" {
		t.Errorf("missing id_token")
	}
}

func TestTokenAuthFallbackPostToBasic(t *testing.T) {
	op := newMockOP(t, "post-fail-then-basic-ok")
	defer op.server.Close()

	svc := NewService(testCfg(op), nil)
	_, flow, _ := svc.StartFlow(t.Context())
	op.nonce = flow.Nonce
	_, oerr := svc.HandleCallback(t.Context(), url.Values{
		"code":  {"c1"},
		"state": {flow.State},
	}, flow)
	if oerr != nil {
		t.Fatalf("first callback: %v", oerr)
	}
	op.mu.Lock()
	first := append([]string(nil), op.seenMethods...)
	op.mu.Unlock()
	if len(first) != 2 || first[0] != "post" || first[1] != "basic" {
		t.Fatalf("auth method sequence = %v, want [post basic]", first)
	}

	// The cached basic method must be used on the second exchange.
	_, flow2, _ := svc.StartFlow(t.Context())
	op.nonce = flow2.Nonce
	if _, oerr := svc.HandleCallback(t.Context(), url.Values{
		"code":  {"c2"},
		"state": {flow2.State},
	}, flow2); oerr != nil {
		t.Fatalf("second callback: %v", oerr)
	}
	op.mu.Lock()
	all := append([]string(nil), op.seenMethods...)
	op.mu.Unlock()
	if len(all) != 3 || all[2] != "basic" {
		t.Fatalf("second exchange did not use cached basic: %v", all)
	}
}

func TestSubjectClaimFallbackOrder(t *testing.T) {
	op := newMockOP(t, "ok")
	defer op.server.Close()
	// No "sub" in the token; subject_claims=["email"] should resolve it.
	op.signHook = func() map[string]any {
		return map[string]any{
			"iss": op.issuer, "aud": "headplane-test",
			"email": "fallback@example.com",
			"iat":   time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		}
	}
	cfg := testCfg(op)
	cfg.SubjectClaims = []string{"email"}
	svc := NewService(cfg, nil)
	_, flow, _ := svc.StartFlow(t.Context())
	op.nonce = flow.Nonce
	ident, oerr := svc.HandleCallback(t.Context(), url.Values{
		"code": {"c1"}, "state": {flow.State},
	}, flow)
	if oerr != nil {
		t.Fatalf("HandleCallback: %v", oerr)
	}
	if ident.Subject != "fallback@example.com" {
		t.Errorf("subject = %q", ident.Subject)
	}
	// "sub" first: with default order and no sub anywhere → missing_sub.
	svc2 := NewService(testCfg(op), nil)
	_, flow2, _ := svc2.StartFlow(t.Context())
	op.nonce = flow2.Nonce
	_, oerr = svc2.HandleCallback(t.Context(), url.Values{
		"code": {"c2"}, "state": {flow2.State},
	}, flow2)
	if oerr == nil || oerr.Code != CodeMissingSub {
		t.Errorf("expected missing_sub, got %v", oerr)
	}
}

func TestGravatarFormula(t *testing.T) {
	op := newMockOP(t, "ok")
	defer op.server.Close()
	cfg := testCfg(op)
	cfg.ProfilePictureSource = "gravatar"
	svc := NewService(cfg, nil)
	_, flow, _ := svc.StartFlow(t.Context())
	op.nonce = flow.Nonce
	ident, oerr := svc.HandleCallback(t.Context(), url.Values{
		"code": {"c1"}, "state": {flow.State},
	}, flow)
	if oerr != nil {
		t.Fatalf("HandleCallback: %v", oerr)
	}
	sum := sha256.Sum256([]byte("mock@example.com")) // trimmed + lowercased
	want := "https://www.gravatar.com/avatar/" + hexOf(sum) + "?s=200&d=identicon&r=x"
	if ident.Picture != want {
		t.Errorf("picture = %q\nwant %q", ident.Picture, want)
	}
}

func hexOf(sum [32]byte) string {
	const hexchars = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range sum {
		out[i*2] = hexchars[b>>4]
		out[i*2+1] = hexchars[b&0xf]
	}
	return string(out)
}

func TestCallbackErrorCases(t *testing.T) {
	op := newMockOP(t, "ok")
	defer op.server.Close()
	svc := NewService(testCfg(op), nil)
	_, flow, _ := svc.StartFlow(t.Context())
	op.nonce = flow.Nonce

	// Provider error param.
	if _, oerr := svc.HandleCallback(t.Context(), url.Values{
		"error":             {"access_denied"},
		"error_description": {"nope"},
	}, flow); oerr == nil || oerr.Code != CodeTokenExchange {
		t.Errorf("provider error: got %v", oerr)
	}
	// Missing code.
	if _, oerr := svc.HandleCallback(t.Context(), url.Values{
		"state": {flow.State},
	}, flow); oerr == nil || oerr.Code != CodeTokenExchange {
		t.Errorf("missing code: got %v", oerr)
	}
	// State mismatch.
	if _, oerr := svc.HandleCallback(t.Context(), url.Values{
		"code": {"c"}, "state": {"wrong"},
	}, flow); oerr == nil || oerr.Code != CodeStateMismatch {
		t.Errorf("state mismatch: got %v", oerr)
	}
	// Nonce mismatch: sign a token with a different nonce.
	op.signHook = func() map[string]any {
		return map[string]any{
			"iss": op.issuer, "aud": "headplane-test", "sub": "u1",
			"nonce": "someone-elses-nonce",
			"iat":   time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		}
	}
	if _, oerr := svc.HandleCallback(t.Context(), url.Values{
		"code": {"c"}, "state": {flow.State},
	}, flow); oerr == nil || oerr.Code != CodeNonceMismatch {
		t.Errorf("nonce mismatch: got %v", oerr)
	}
}

func TestClockTolerance(t *testing.T) {
	op := newMockOP(t, "ok")
	defer op.server.Close()
	svc := NewService(testCfg(op), nil)
	_, flow, _ := svc.StartFlow(t.Context())
	op.nonce = flow.Nonce

	// exp 30s in the past: within 60s tolerance → valid.
	op.signHook = func() map[string]any {
		return map[string]any{
			"iss": op.issuer, "aud": "headplane-test", "sub": "u1",
			"iat": time.Now().Add(-time.Hour).Unix(), "exp": time.Now().Add(-30 * time.Second).Unix(),
		}
	}
	if _, oerr := svc.HandleCallback(t.Context(), url.Values{
		"code": {"c1"}, "state": {flow.State},
	}, flow); oerr != nil {
		t.Errorf("tolerance case: %v", oerr)
	}
	// exp 61s in the past: beyond tolerance → expired.
	op.signHook = func() map[string]any {
		return map[string]any{
			"iss": op.issuer, "aud": "headplane-test", "sub": "u1",
			"iat": time.Now().Add(-time.Hour).Unix(), "exp": time.Now().Add(-61 * time.Second).Unix(),
		}
	}
	_, flow2, _ := svc.StartFlow(t.Context())
	op.nonce = flow2.Nonce
	if _, oerr := svc.HandleCallback(t.Context(), url.Values{
		"code": {"c2"}, "state": {flow2.State},
	}, flow2); oerr == nil || oerr.Code != CodeInvalidIDToken || !strings.Contains(oerr.Message, "expired") {
		t.Errorf("expired case: got %v", oerr)
	}
}

func TestWeakRSAKeys(t *testing.T) {
	// Build an OP with a 1024-bit RSA key.
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	op := newMockOP(t, "ok")
	op.key = key
	defer op.server.Close()

	// Rejected by default.
	svc := NewService(testCfg(op), nil)
	_, flow, _ := svc.StartFlow(t.Context())
	op.nonce = flow.Nonce
	if _, oerr := svc.HandleCallback(t.Context(), url.Values{
		"code": {"c1"}, "state": {flow.State},
	}, flow); oerr == nil || oerr.Code != CodeInvalidIDToken ||
		!strings.Contains(oerr.Message, "weak RSA key") {
		t.Errorf("weak key default: got %v", oerr)
	}

	// Accepted with allow_weak_rsa_keys.
	cfg := testCfg(op)
	cfg.AllowWeakRSAKeys = true
	svc2 := NewService(cfg, nil)
	_, flow2, _ := svc2.StartFlow(t.Context())
	op.nonce = flow2.Nonce
	ident, oerr := svc2.HandleCallback(t.Context(), url.Values{
		"code": {"c2"}, "state": {flow2.State},
	}, flow2)
	if oerr != nil {
		t.Fatalf("weak key allowed: %v", oerr)
	}
	if ident.Subject != "mock-user-1" {
		t.Errorf("identity = %+v", ident)
	}
}

func TestUserinfoEnrichment(t *testing.T) {
	op := newMockOP(t, "ok")
	defer op.server.Close()
	// ID token has only sub; name/email come from userinfo.
	op.signHook = func() map[string]any {
		return map[string]any{
			"iss": op.issuer, "aud": "headplane-test", "sub": "bare-sub",
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		}
	}
	op.userinfo = map[string]any{
		"sub": "bare-sub", "name": "Enriched Name", "email": "enriched@example.com",
	}
	svc := NewService(testCfg(op), nil)
	_, flow, _ := svc.StartFlow(t.Context())
	op.nonce = flow.Nonce
	ident, oerr := svc.HandleCallback(t.Context(), url.Values{
		"code": {"c1"}, "state": {flow.State},
	}, flow)
	if oerr != nil {
		t.Fatalf("HandleCallback: %v", oerr)
	}
	if ident.Name != "Enriched Name" || ident.Email != "enriched@example.com" {
		t.Errorf("identity = %+v", ident)
	}
}

func TestEndSessionURL(t *testing.T) {
	op := newMockOP(t, "ok")
	defer op.server.Close()
	svc := NewService(testCfg(op), nil)
	endURL := svc.BuildEndSessionURL(t.Context(), "id-token-abc")
	u, err := url.Parse(endURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != op.server.URL+"/end" {
		t.Errorf("end session endpoint = %s", endURL)
	}
	q := u.Query()
	if q.Get("id_token_hint") != "id-token-abc" {
		t.Errorf("id_token_hint = %q", q.Get("id_token_hint"))
	}
	if q.Get("client_id") != "headplane-test" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("post_logout_redirect_uri") != "http://localhost:3000/admin/login?s=logout" {
		t.Errorf("post_logout_redirect_uri = %q", q.Get("post_logout_redirect_uri"))
	}
}

func TestManualEndpointsSkipDiscovery(t *testing.T) {
	op := newMockOP(t, "ok")
	defer op.server.Close()
	cfg := testCfg(op)
	// Point discovery at a dead URL; manual endpoints must take precedence.
	cfg.Issuer = "http://127.0.0.1:1/dead"
	cfg.AuthorizationEndpoint = op.server.URL + "/authorize"
	cfg.TokenEndpoint = op.server.URL + "/token"
	cfg.JWKsURI = op.server.URL + "/jwks"
	svc := NewService(cfg, nil)
	ep, oerr := svc.Discover(t.Context())
	if oerr != nil {
		t.Fatalf("Discover: %v", oerr)
	}
	if ep.AuthorizationEndpoint != op.server.URL+"/authorize" {
		t.Errorf("endpoints = %+v", ep)
	}
}
