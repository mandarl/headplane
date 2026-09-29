package passkey

import (
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"
)

func testService(t *testing.T) *Service {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE users (id TEXT PRIMARY KEY, name TEXT, sub TEXT)`,
		`CREATE TABLE webauthn_credentials (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, credential_id TEXT NOT NULL UNIQUE,
			public_key BLOB NOT NULL, attestation_type TEXT NOT NULL DEFAULT '',
			transports TEXT NOT NULL DEFAULT '', backup_eligible INTEGER NOT NULL DEFAULT 0,
			backup_state INTEGER NOT NULL DEFAULT 0, label TEXT NOT NULL DEFAULT '',
			sign_count INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, last_used_at INTEGER
		)`,
		`INSERT INTO users (id, name, sub) VALUES ('user-1', 'Test User', 'sub-1')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return NewService(db, "", slog.New(slog.NewTextHandler(discard{}, nil)))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func requestWithHost(t *testing.T, host string, tlsOn bool, proto string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://"+host+"/x", nil)
	r.Host = host
	if tlsOn {
		r.TLS = &tls.ConnectionState{}
	}
	if proto != "" {
		r.Header.Set("X-Forwarded-Proto", proto)
	}
	return r
}

func TestRPIDForRequest(t *testing.T) {
	svc := testService(t)

	if got := svc.RPIDForRequest(requestWithHost(t, "headplane.example.com", true, "")); got != "headplane.example.com" {
		t.Errorf("RPID = %q, want host", got)
	}
	if got := svc.RPIDForRequest(requestWithHost(t, "headplane.example.com:3000", true, "")); got != "headplane.example.com" {
		t.Errorf("RPID with port = %q, want port stripped", got)
	}

	// Explicit override wins over the request host.
	ov := NewService(testService(t).db, "login.example.com", slog.New(slog.NewTextHandler(discard{}, nil)))
	if got := ov.RPIDForRequest(requestWithHost(t, "other.example.com", true, "")); got != "login.example.com" {
		t.Errorf("RPID override = %q, want login.example.com", got)
	}
}

func TestOriginForRequest(t *testing.T) {
	cases := []struct {
		name  string
		host  string
		tlsOn bool
		proto string
		want  string
	}{
		{"tls", "example.com", true, "", "https://example.com"},
		{"plain", "example.com", false, "", "http://example.com"},
		{"forwarded https", "example.com", false, "https", "https://example.com"},
		{"forwarded http", "example.com", true, "http", "https://example.com"}, // TLS wins
	}
	for _, c := range cases {
		if got := originForRequest(requestWithHost(t, c.host, c.tlsOn, c.proto)); got != c.want {
			t.Errorf("%s: origin = %q, want %q", c.name, got, c.want)
		}
	}
}

func marshalJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestRegistrationOptionsCeremonyParams(t *testing.T) {
	svc := testService(t)
	r := requestWithHost(t, "example.com", true, "")
	_, creation, err := svc.BeginRegistration("user-1", r)
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	m := marshalJSON(t, creation.Response)

	if got := m["attestation"]; got != "none" {
		t.Errorf("attestation = %v, want none", got)
	}
	as, ok := m["authenticatorSelection"].(map[string]any)
	if !ok {
		t.Fatalf("authenticatorSelection missing: %v", m)
	}
	// Discoverable credentials: residentKey required, no attachment pin.
	if got := as["residentKey"]; got != "required" {
		t.Errorf("residentKey = %v, want required", got)
	}
	if got := as["requireResidentKey"]; got != true {
		t.Errorf("requireResidentKey = %v, want true", got)
	}
	if _, present := as["authenticatorAttachment"]; present {
		t.Errorf("authenticatorAttachment = %v, want absent (no restriction)", as["authenticatorAttachment"])
	}
	if got := as["userVerification"]; got != "preferred" {
		t.Errorf("userVerification = %v, want preferred", got)
	}
	if got := m["timeout"]; got != float64(120000) {
		t.Errorf("timeout = %v, want 120000", got)
	}
	hints, ok := m["hints"].([]any)
	if !ok || len(hints) != 1 || hints[0] != "hybrid" {
		t.Errorf("hints = %v, want [\"hybrid\"]", m["hints"])
	}
}

func TestLoginOptionsCeremonyParams(t *testing.T) {
	svc := testService(t)
	r := requestWithHost(t, "example.com", true, "")
	_, assertion, err := svc.BeginLogin(r)
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	m := marshalJSON(t, assertion.Response)

	// Usernameless: allowCredentials must be absent or empty.
	if raw, present := m["allowCredentials"]; present {
		if list, ok := raw.([]any); !ok || len(list) != 0 {
			t.Errorf("allowCredentials = %v, want absent or empty", raw)
		}
	}
	if got := m["userVerification"]; got != "preferred" {
		t.Errorf("userVerification = %v, want preferred", got)
	}
	if got := m["timeout"]; got != float64(120000) {
		t.Errorf("timeout = %v, want 120000", got)
	}
	hints, ok := m["hints"].([]any)
	if !ok || len(hints) != 1 || hints[0] != "hybrid" {
		t.Errorf("hints = %v, want [\"hybrid\"]", m["hints"])
	}
}

func TestChallengeTokenSingleUse(t *testing.T) {
	svc := testService(t)
	r := requestWithHost(t, "example.com", true, "")
	token, _, err := svc.BeginLogin(r)
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	if _, err := svc.takeChallenge(token, "login"); err != nil {
		t.Fatalf("first take: %v", err)
	}
	if _, err := svc.takeChallenge(token, "login"); err == nil {
		t.Fatalf("second take of the same token should fail")
	}
	if _, err := svc.takeChallenge(token, "register"); err == nil {
		t.Fatalf("taking a login token as a register token should fail")
	}
}

func TestChallengeTokenExpiry(t *testing.T) {
	svc := testService(t)
	r := requestWithHost(t, "example.com", true, "")
	token, _, err := svc.BeginLogin(r)
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	// Expire the entry by backdating it, then prune.
	svc.mu.Lock()
	if e, ok := svc.challenges[token]; ok {
		e.expires = e.expires.Add(-2 * challengeTTL)
	}
	svc.mu.Unlock()
	svc.prune()
	if _, err := svc.takeChallenge(token, "login"); err == nil {
		t.Fatalf("expired token should fail")
	}
}

func TestChallengeTokenNotFound(t *testing.T) {
	svc := testService(t)
	if _, err := svc.takeChallenge("bogus", "login"); err == nil {
		t.Fatalf("unknown token should fail")
	}
}

func insertCredential(t *testing.T, svc *Service, id, userID, credID, label string) {
	t.Helper()
	_, err := svc.db.Exec(
		`INSERT INTO webauthn_credentials
		 (id, user_id, credential_id, public_key, attestation_type, transports,
		  backup_eligible, backup_state, label, sign_count, created_at, updated_at)
		 VALUES (?, ?, ?, x'00', 'none', '["hybrid"]', 1, 1, ?, 0,
		         strftime('%s','now'), strftime('%s','now'))`,
		id, userID, credID, label,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCredentialCRUD(t *testing.T) {
	svc := testService(t)
	if svc.HasAny("user-1") {
		t.Fatalf("HasAny should be false before any credential")
	}
	insertCredential(t, svc, "row-1", "user-1", "Y3JlZC0x", "My key")
	if !svc.HasAny("user-1") {
		t.Fatalf("HasAny should be true after insert")
	}

	if err := svc.UpdateLabel("user-1", "Y3JlZC0x", "Renamed"); err != nil {
		t.Fatalf("UpdateLabel: %v", err)
	}
	var label string
	if err := svc.db.QueryRow(`SELECT label FROM webauthn_credentials WHERE id='row-1'`).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label != "Renamed" {
		t.Errorf("label = %q, want Renamed", label)
	}

	// Cross-user writes must fail: user-2 does not exist and owns nothing.
	if err := svc.UpdateLabel("user-2", "Y3JlZC0x", "Hacked"); err == nil {
		t.Errorf("UpdateLabel by another user should fail")
	}
	if err := svc.Delete("user-2", "Y3JlZC0x"); err == nil {
		t.Errorf("Delete by another user should fail")
	}

	if err := svc.Delete("user-1", "Y3JlZC0x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if svc.HasAny("user-1") {
		t.Errorf("HasAny should be false after delete")
	}
	if err := svc.Delete("user-1", "Y3JlZC0x"); err == nil {
		t.Errorf("deleting a missing credential should fail")
	}
}

func TestListCredentials(t *testing.T) {
	svc := testService(t)
	insertCredential(t, svc, "row-1", "user-1", "Y3JlZC0x", "First")
	insertCredential(t, svc, "row-2", "user-1", "Y3JlZC0y", "")
	list, err := svc.List("user-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %d credentials, want 2", len(list))
	}
	labels := map[string]bool{}
	for _, c := range list {
		labels[c.Label] = true
	}
	if !labels["First"] || !labels[""] {
		t.Errorf("labels missing; got %v", labels)
	}
	for _, c := range list {
		if len(c.Transports) != 1 || c.Transports[0] != "hybrid" {
			t.Errorf("transports = %v, want [hybrid]", c.Transports)
		}
		if !c.BackupEligible || !c.BackupState {
			t.Errorf("backup flags = %v/%v, want true/true", c.BackupEligible, c.BackupState)
		}
	}
}
