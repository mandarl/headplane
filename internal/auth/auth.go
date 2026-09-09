package auth

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Principal mirrors the TS Principal union. Exported so the HTTP
// layer can branch on the principal kind (e.g. OIDC RP-initiated logout).
type Principal struct {
	Kind      string // "api_key" | "oidc"
	SessionID string

	// api_key principals
	DisplayName string
	APIKey      string

	// oidc principals
	IDToken         *string
	UserID          string
	Subject         string
	Role            Role
	HeadscaleUserID *string
	ProfileName     string
	ProfileEmail    *string
	ProfileUsername *string
	ProfilePicture  *string
}

// pruneInterval mirrors the TS 15-minute expired-session prune timer.
const pruneInterval = 15 * time.Minute

// Service is the Go port of the TS AuthService (app/server/web/auth.ts).
type Service struct {
	db              *sql.DB
	secret          string
	headscaleAPIKey string
	cookieOpts      CookieOptions

	stopCh  chan struct{}
	stopped chan struct{}
	running atomic.Bool
}

// OpenDB opens (creating parent dirs like the TS createDbClient) the SQLite
// database at path. It deliberately runs no migrations and sets no journal
// mode: the file is owned by the same schema the Node server writes, and
// node-sqlite's defaults (rollback journal) are left untouched so a DB file
// written by either server stays mutually readable. A busy timeout keeps
// the prune loop from tripping over an in-flight request.
func OpenDB(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("auth: cannot create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+abs+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// NewService builds the auth service. cookieOpts carries the _hp_auth cookie
// parameters (name "_hp_auth", path = basename, maxAge = server.cookie_max_age
// seconds, secure, domain) exactly as app/server/context.ts configures them.
func NewService(db *sql.DB, secret, headscaleAPIKey string, cookieOpts CookieOptions) *Service {
	return &Service{
		db:              db,
		secret:          secret,
		headscaleAPIKey: headscaleAPIKey,
		cookieOpts:      cookieOpts,
		stopCh:          make(chan struct{}),
		stopped:         make(chan struct{}),
	}
}

type sessionRow struct {
	id            string
	kind          string
	userID        sql.NullString
	apiKeyHash    sql.NullString
	apiKeyDisplay sql.NullString
	oidcIDToken   sql.NullString
	expiresAt     int64
}

type userRow struct {
	id              string
	sub             string
	name            sql.NullString
	email           sql.NullString
	picture         sql.NullString
	role            string
	headscaleUserID sql.NullString
}

// DB exposes the underlying database handle for tests and for server
// wiring that needs direct access.
func (s *Service) DB() *sql.DB { return s.db }

// Require validates the request's session cookie and returns the principal,
// mirroring the TS resolve(). An expired session row is deleted before the
// error is returned, exactly like the TS implementation.
func (s *Service) Require(r *http.Request) (*Principal, error) {
	payload, err := VerifyCookie(s.secret, r.Header.Get("Cookie"), s.cookieOpts.Name)
	if err != nil {
		return nil, err
	}

	var sess sessionRow
	err = s.db.QueryRow(`SELECT id, kind, user_id, api_key_hash, api_key_display, oidc_id_token, expires_at
		FROM auth_sessions WHERE id = ?`, payload.SID).Scan(
		&sess.id, &sess.kind, &sess.userID, &sess.apiKeyHash, &sess.apiKeyDisplay, &sess.oidcIDToken, &sess.expiresAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("auth: session not found")
	}
	if err != nil {
		return nil, err
	}

	if sess.expiresAt < time.Now().Unix() {
		_, _ = s.db.Exec(`DELETE FROM auth_sessions WHERE id = ?`, sess.id)
		return nil, fmt.Errorf("auth: session expired")
	}

	if sess.kind == "api_key" {
		if payload.APIKey == "" {
			return nil, fmt.Errorf("auth: API key session missing credential")
		}
		display := "API Key"
		if sess.apiKeyDisplay.Valid {
			display = sess.apiKeyDisplay.String
		}
		return &Principal{
			Kind:        "api_key",
			SessionID:   sess.id,
			DisplayName: display,
			APIKey:      payload.APIKey,
		}, nil
	}

	if !sess.userID.Valid {
		return nil, fmt.Errorf("auth: OIDC session missing user_id")
	}

	var user userRow
	err = s.db.QueryRow(`SELECT id, sub, name, email, picture, role, headscale_user_id
		FROM users WHERE id = ?`, sess.userID.String).Scan(
		&user.id, &user.sub, &user.name, &user.email, &user.picture, &user.role, &user.headscaleUserID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("auth: user record not found")
	}
	if err != nil {
		return nil, err
	}

	p := &Principal{
		Kind:      "oidc",
		SessionID: sess.id,
		UserID:    user.id,
		Subject:   user.sub,
		Role:      NormalizeRole(user.role),
	}
	if sess.oidcIDToken.Valid {
		tok := sess.oidcIDToken.String
		p.IDToken = &tok
	}
	if user.headscaleUserID.Valid {
		hs := user.headscaleUserID.String
		p.HeadscaleUserID = &hs
	}
	// Profile fallbacks mirror the TS resolve(): cookie profile first, then
	// the user record, then the subject.
	p.ProfileName = user.sub
	if user.name.Valid {
		p.ProfileName = user.name.String
	}
	if payload.Profile != nil && payload.Profile.Name != "" {
		p.ProfileName = payload.Profile.Name
	}
	if user.email.Valid {
		e := user.email.String
		p.ProfileEmail = &e
	}
	if payload.Profile != nil && payload.Profile.Email != nil {
		p.ProfileEmail = payload.Profile.Email
	}
	if payload.Profile != nil && payload.Profile.Username != nil {
		p.ProfileUsername = payload.Profile.Username
	}
	if user.picture.Valid {
		pic := user.picture.String
		p.ProfilePicture = &pic
	}
	return p, nil
}

// Can mirrors the TS can(): api_key principals bypass all role checks.
func (s *Service) Can(p *Principal, caps Capabilities) bool {
	if p.Kind == "api_key" {
		return true
	}
	return CapsForRole(string(p.Role))&caps == caps
}

// CanManageNode mirrors the TS canManageNode(). nodeHeadscaleUserID is the
// Headscale user id owning the node (nil when the node has no owner).
func (s *Service) CanManageNode(p *Principal, nodeHeadscaleUserID *string) bool {
	if p.Kind == "api_key" {
		return true
	}
	if CapsForRole(string(p.Role))&CapWriteMachines != 0 {
		return true
	}
	return p.HeadscaleUserID != nil && nodeHeadscaleUserID != nil &&
		*p.HeadscaleUserID == *nodeHeadscaleUserID
}

// GetHeadscaleAPIKey mirrors the TS getHeadscaleApiKey(): api_key principals
// use the key from their session; OIDC principals use the configured
// headscale.api_key.
func (s *Service) GetHeadscaleAPIKey(p *Principal) (string, error) {
	if p.Kind == "api_key" {
		return p.APIKey, nil
	}
	if s.headscaleAPIKey == "" {
		return "", fmt.Errorf("auth: OIDC sessions require headscale.api_key to be configured")
	}
	return s.headscaleAPIKey, nil
}

// CreateOidcSession inserts the session row and returns the Set-Cookie header
// value. maxAge is the session lifetime in the same unit as the TS
// createOidcSession maxAge option (seconds); the cookie Max-Age is whole
// seconds, mirroring encodeCookie(payload, maxAge). expires_at/created_at
// are stored as whole seconds, matching drizzle's timestamp mode.
func (s *Service) CreateOidcSession(userID string, profile CookieProfile, idToken *string, maxAge time.Duration) (string, error) {
	sid := NewULID()
	now := time.Now()
	expiresAt := now.Add(maxAge).Unix()
	var idTok sql.NullString
	if idToken != nil {
		idTok = sql.NullString{String: *idToken, Valid: true}
	}
	_, err := s.db.Exec(`INSERT INTO auth_sessions (id, kind, user_id, oidc_id_token, expires_at, created_at)
		VALUES (?, 'oidc', ?, ?, ?, ?)`, sid, userID, idTok, expiresAt, now.Unix())
	if err != nil {
		return "", err
	}
	value, err := SignCookie(s.secret, CookiePayload{SID: sid, Profile: &profile})
	if err != nil {
		return "", err
	}
	opts := s.cookieOpts
	opts.MaxAge = int(maxAge.Seconds())
	return SerializeSetCookie(opts, value, nil), nil
}

// CreateAPIKeySession inserts the session row and returns the Set-Cookie
// header value. maxAge is the session lifetime in the same unit as the TS
// createApiKeySession maxAge argument (a time.Duration carrying
// milliseconds); the cookie Max-Age is floor(maxAge/1000) seconds, mirroring
// the TS encodeCookie({sid, api_key}, Math.floor(maxAge / 1000)).
func (s *Service) CreateAPIKeySession(apiKey, displayName string, maxAge time.Duration) (string, error) {
	sid := NewULID()
	now := time.Now()
	expiresAt := now.Add(maxAge).Unix()
	_, err := s.db.Exec(`INSERT INTO auth_sessions (id, kind, api_key_hash, api_key_display, expires_at, created_at)
		VALUES (?, 'api_key', ?, ?, ?, ?)`, sid, HashAPIKey(apiKey), displayName, expiresAt, now.Unix())
	if err != nil {
		return "", err
	}
	value, err := SignCookie(s.secret, CookiePayload{SID: sid, APIKey: apiKey})
	if err != nil {
		return "", err
	}
	opts := s.cookieOpts
	opts.MaxAge = int(maxAge.Milliseconds() / 1000)
	return SerializeSetCookie(opts, value, nil), nil
}

// DestroySession deletes the request's session (best effort, like the TS
// catch-and-clear) and returns the session-clearing Set-Cookie header value.
func (s *Service) DestroySession(r *http.Request) string {
	if r != nil {
		if payload, err := VerifyCookie(s.secret, r.Header.Get("Cookie"), s.cookieOpts.Name); err == nil {
			_, _ = s.db.Exec(`DELETE FROM auth_sessions WHERE id = ?`, payload.SID)
		}
	}
	return ClearCookieValue(s.cookieOpts)
}

// PruneExpiredSessions deletes expired sessions, mirroring the TS
// pruneExpiredSessions (lt(expires_at, now); expires_at is whole seconds).
func (s *Service) PruneExpiredSessions() error {
	_, err := s.db.Exec(`DELETE FROM auth_sessions WHERE expires_at < ?`, time.Now().Unix())
	return err
}

// Start begins the 15-minute prune loop. It is idempotent; Stop ends the
// loop and is safe to call without Start.
func (s *Service) Start() {
	if !s.running.CompareAndSwap(false, true) {
		return
	}
	ticker := time.NewTicker(pruneInterval)
	go func() {
		defer ticker.Stop()
		defer close(s.stopped)
		for {
			select {
			case <-ticker.C:
				_ = s.PruneExpiredSessions()
			case <-s.stopCh:
				return
			}
		}
	}()
}

// Stop halts the prune loop, mirroring the TS stop(). Safe to call without
// Start and safe to call twice.
func (s *Service) Stop() {
	if !s.running.Load() {
		return
	}
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	<-s.stopped
}
