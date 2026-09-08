package auth

import (
	"database/sql"
	"net/http"
	"testing"
	"time"
)

// Permanent cross-implementation pin: this cookie value was issued by the
// real TypeScript createAuthService (node:crypto HMAC-SHA256, react-router
// createCookie wrapping) with the secret below. It pins the byte-identical
// _hp_auth scheme: if the TS side ever changes its encoding, this test
// fails. The session row it refers to is inserted by the test itself with a
// fresh expiry, since the cookie signature does not cover expires_at.
const (
	nodeInteropSecret = "interop-secret-0123456789abcdefX" // exactly 32 chars
	nodeInteropSID    = "01M1ZDR3ZNZZKMXREZXH3VMNBE"
	nodeInteropAPIKey = "hskey-interop-raw-key"
	// Issued by: createApiKeySession("hskey-interop-raw-key", "interop-key...", 3600_000)
	// Decodes (percent-decode, base64, JSON-unquote) to
	//   {"sid":"01M1ZDR3ZNZZKMXREZXH3VMNBE","api_key":"hskey-interop-raw-key"}.<hmac>
	nodeInteropCookieValue = "ImV5SnphV1FpT2lJd01VMHhXa1JTTTFwT1dscExUVmhTUlZwWVNETldUVTVDUlNJc0ltRndhVjlyWlhraU9pSm9jMnRsZVMxcGJuUmxjbTl3TFhKaGR5MXJaWGtpZlEuZFdUZHBWV2Z2Zi1nTzMwY29wUV9YOUtqV0VieF9XbkxvVTMzQ1MyMmNFbyI%3D"
)

func TestNodeIssuedCookieVerifies(t *testing.T) {
	payload, err := VerifyCookie(nodeInteropSecret, "_hp_auth="+nodeInteropCookieValue, "_hp_auth")
	if err != nil {
		t.Fatalf("node-issued cookie rejected: %v", err)
	}
	if payload.SID != nodeInteropSID {
		t.Fatalf("sid mismatch: %q", payload.SID)
	}
	// API-key principals carry the raw key inside the signed cookie.
	if payload.APIKey != nodeInteropAPIKey {
		t.Fatalf("api_key mismatch: %q", payload.APIKey)
	}

	if _, err := VerifyCookie("wrong-secret-wrong-secret-123456", "_hp_auth="+nodeInteropCookieValue, "_hp_auth"); err == nil {
		t.Fatal("node-issued cookie accepted with wrong secret")
	}
}

func TestNodeIssuedSessionResolves(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(testSchema); err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, nodeInteropSecret, "hskey-configured", CookieOptions{
		Name: "_hp_auth", Path: "/admin", MaxAge: 86400,
	})
	// Mirror the row the TS service wrote: api_key session with a fresh
	// expiry (drizzle timestamp mode stores whole seconds).
	if _, err := svc.db.Exec(`INSERT INTO auth_sessions
		(id, kind, api_key_hash, api_key_display, expires_at, created_at)
		VALUES (?, 'api_key', ?, ?, ?, ?)`,
		nodeInteropSID, HashAPIKey(nodeInteropAPIKey), "interop-key...",
		time.Now().Add(time.Hour).Unix(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("GET", "/admin/machines", nil)
	req.Header.Set("Cookie", "_hp_auth="+nodeInteropCookieValue)
	p, err := svc.Require(req)
	if err != nil {
		t.Fatalf("node-issued session rejected: %v", err)
	}
	if p.Kind != "api_key" || p.SessionID != nodeInteropSID {
		t.Fatalf("unexpected principal: %+v", p)
	}
	if p.APIKey != nodeInteropAPIKey {
		t.Fatalf("principal api key mismatch: %q", p.APIKey)
	}
	if p.DisplayName != "interop-key..." {
		t.Fatalf("principal display mismatch: %q", p.DisplayName)
	}
	// API-key principals bypass role checks.
	if !svc.Can(p, CapWritePolicy|CapConfigureIAM) {
		t.Fatal("api_key principal should bypass capability checks")
	}
}
