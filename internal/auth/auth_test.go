package auth

import (
	"database/sql"
	"net/http"
	"testing"
	"time"
)

const testSchema = `
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
);`

func testService(t *testing.T) *Service {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(testSchema); err != nil {
		t.Fatal(err)
	}
	opts := CookieOptions{Name: "_hp_auth", Path: "/admin", MaxAge: 86400, Secure: false}
	return NewService(db, testSecret, "hskey-configured", opts)
}

func cookieHeader(t *testing.T, setCookie string) string {
	t.Helper()
	// "name=value; ..." -> "name=value"
	if i := indexByte(setCookie, ';'); i >= 0 {
		return setCookie[:i]
	}
	return setCookie
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func TestAPIKeySessionRoundTrip(t *testing.T) {
	svc := testService(t)
	setCookie, err := svc.CreateAPIKeySession("test-prefix.secret", "test-prefix...", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("GET", "/", nil)
	req.Header.Set("Cookie", cookieHeader(t, setCookie))
	p, err := svc.Require(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "api_key" || p.APIKey != "test-prefix.secret" || p.DisplayName != "test-prefix..." {
		t.Fatalf("unexpected principal: %+v", p)
	}
	if !svc.Can(p, CapOwner) {
		t.Fatal("api_key should bypass capability checks")
	}
	if key, err := svc.GetHeadscaleAPIKey(p); err != nil || key != "test-prefix.secret" {
		t.Fatalf("GetHeadscaleAPIKey = %q, %v", key, err)
	}

	// The raw key must be recoverable from the cookie payload only; the DB
	// stores just the hash.
	var hash string
	if err := svc.db.QueryRow(`SELECT api_key_hash FROM auth_sessions`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != HashAPIKey("test-prefix.secret") {
		t.Fatalf("api_key_hash mismatch: %q", hash)
	}
}

func TestExpiredSessionDeletedOnAccess(t *testing.T) {
	svc := testService(t)
	setCookie, err := svc.CreateAPIKeySession("k", "k...", -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "/", nil)
	req.Header.Set("Cookie", cookieHeader(t, setCookie))
	if _, err := svc.Require(req); err == nil {
		t.Fatal("expired session accepted")
	}
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("expired session row was not deleted on access")
	}
}

func TestOidcSessionRoundTrip(t *testing.T) {
	svc := testService(t)
	now := time.Now().Unix()
	if _, err := svc.db.Exec(`INSERT INTO users (id, sub, name, email, role, created_at) VALUES (?,?,?,?,?,?)`,
		"01USER", "subject-123", "Jane", "jane@example.com", "admin", now); err != nil {
		t.Fatal(err)
	}
	email := "jane@example.com"
	setCookie, err := svc.CreateOidcSession("01USER", CookieProfile{Name: "Jane", Email: &email}, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("GET", "/", nil)
	req.Header.Set("Cookie", cookieHeader(t, setCookie))
	p, err := svc.Require(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "oidc" || p.UserID != "01USER" || p.Subject != "subject-123" || p.Role != RoleAdmin {
		t.Fatalf("unexpected principal: %+v", p)
	}
	if p.ProfileName != "Jane" || p.ProfileEmail == nil || *p.ProfileEmail != "jane@example.com" {
		t.Fatalf("unexpected profile: %+v", p)
	}
	if key, err := svc.GetHeadscaleAPIKey(p); err != nil || key != "hskey-configured" {
		t.Fatalf("OIDC principal should use configured key, got %q, %v", key, err)
	}
}

func TestOidcUnknownRoleFallsBackToMember(t *testing.T) {
	svc := testService(t)
	now := time.Now().Unix()
	if _, err := svc.db.Exec(`INSERT INTO users (id, sub, role, created_at) VALUES (?,?,?,?)`,
		"01USER", "s", "superadmin", now); err != nil {
		t.Fatal(err)
	}
	setCookie, err := svc.CreateOidcSession("01USER", CookieProfile{Name: "S"}, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "/", nil)
	req.Header.Set("Cookie", cookieHeader(t, setCookie))
	p, err := svc.Require(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Role != RoleMember {
		t.Fatalf("unknown role should normalize to member, got %q", p.Role)
	}
}

func TestDestroySession(t *testing.T) {
	svc := testService(t)
	setCookie, err := svc.CreateAPIKeySession("k", "k...", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", "/logout", nil)
	req.Header.Set("Cookie", cookieHeader(t, setCookie))

	clear := svc.DestroySession(req)
	if clear != "_hp_auth=; Max-Age=86400; Path=/admin; Expires=Thu, 01 Jan 1970 00:00:00 GMT; SameSite=Lax" {
		t.Fatalf("unexpected clear header: %q", clear)
	}
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("session row not deleted by DestroySession")
	}
	// Invalid cookie still yields a clearing header.
	bad, _ := http.NewRequest("POST", "/logout", nil)
	bad.Header.Set("Cookie", "_hp_auth=bogus")
	if c := svc.DestroySession(bad); c != clear {
		t.Fatalf("bad cookie should still clear, got %q", c)
	}
}

func TestPruneExpiredSessions(t *testing.T) {
	svc := testService(t)
	if _, err := svc.CreateAPIKeySession("fresh", "f", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateAPIKeySession("stale", "s", -time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := svc.PruneExpiredSessions(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 session after prune, got %d", n)
	}
}
