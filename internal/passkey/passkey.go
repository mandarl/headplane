// Package passkey implements WebAuthn (passkey) registration and login for
// headplane. It wraps github.com/go-webauthn/webauthn with headplane's
// storage conventions (SQLite via database/sql, ULID ids, whole-second
// timestamps) and the ceremony parameters Mandar approved:
//
//   - Discoverable credentials: residentKey "required" / requireResidentKey.
//   - Empty allowCredentials at login (usernameless), with hints ["hybrid"]
//     so a phone can complete the ceremony over QR when the computer has no
//     passkey provider (e.g. no LastPass).
//   - No authenticatorAttachment restriction.
//   - userVerification "preferred" (not strict).
//   - attestation "none".
//   - ~120 second ceremony timeouts.
//
// The relying-party ID defaults to the incoming request's host (so it works
// out of the box behind any hostname) and can be pinned with the
// webauthn.rp_id config key for reverse-proxy deployments where the public
// hostname differs.
//
// Ceremony challenges live in an in-memory store keyed by a random token
// (5-minute TTL). This is single-instance state: like the session prune
// loop, it assumes one server process.
package passkey

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// RPName is the WebAuthn relying-party display name shown by authenticators.
const RPName = "headplane"

// ceremonyTimeout is the per-ceremony timeout surfaced to the browser and
// enforced server-side, per the approved design (~120 seconds).
const ceremonyTimeout = 120 * time.Second

// challengeTTL bounds how long a ceremony token stays valid server-side.
const challengeTTL = 5 * time.Minute

// CredentialInfo is the JSON shape served for a registered passkey.
type CredentialInfo struct {
	ID             string   `json:"id"`
	Label          string   `json:"label"`
	Transports     []string `json:"transports"`
	BackupEligible bool     `json:"backup_eligible"`
	BackupState    bool     `json:"backup_state"`
	CreatedAt      int64    `json:"created_at"`
	LastUsedAt     *int64   `json:"last_used_at,omitempty"`
}

// challengeEntry is one in-flight ceremony.
type challengeEntry struct {
	session webauthn.SessionData
	kind    string // "registration" | "login"
	userID  string // registration only
	expires time.Time
}

// Service owns passkey storage and ceremony state.
type Service struct {
	db           *sql.DB
	rpIDOverride string
	logger       *slog.Logger

	mu         sync.Mutex
	challenges map[string]*challengeEntry
	stopCh     chan struct{}
}

// NewService builds the passkey service. rpIDOverride is the webauthn.rp_id
// config value ("" means derive from the request host).
func NewService(db *sql.DB, rpIDOverride string, logger *slog.Logger) *Service {
	s := &Service{
		db:           db,
		rpIDOverride: strings.ToLower(strings.TrimSpace(rpIDOverride)),
		logger:       logger.With("component", "passkey"),
		challenges:   map[string]*challengeEntry{},
		stopCh:       make(chan struct{}),
	}
	go s.pruneLoop()
	return s
}

// Stop ends the challenge prune loop.
func (s *Service) Stop() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
}

func (s *Service) pruneLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.prune()
		case <-s.stopCh:
			return
		}
	}
}

func (s *Service) prune() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, e := range s.challenges {
		if now.After(e.expires) {
			delete(s.challenges, token)
		}
	}
}

// RPIDForRequest returns the relying-party ID for a ceremony: the
// webauthn.rp_id override when configured, otherwise the request host
// without any port, lowercased.
func (s *Service) RPIDForRequest(r *http.Request) string {
	if s.rpIDOverride != "" {
		return s.rpIDOverride
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(host)
}

// originForRequest derives the expected ceremony origin from the request.
// X-Forwarded-Proto is honored so TLS-terminating reverse proxies work.
func originForRequest(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.ToLower(strings.Split(proto, ",")[0])
	}
	return scheme + "://" + r.Host
}

// webAuthnFor builds a per-request WebAuthn instance bound to the request's
// RP ID and origin.
func (s *Service) webAuthnFor(r *http.Request) (*webauthn.WebAuthn, error) {
	rpID := s.RPIDForRequest(r)
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: RPName,
		RPOrigins:     []string{originForRequest(r)},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Timeout: ceremonyTimeout, TimeoutUVD: ceremonyTimeout, Enforce: true},
			Registration: webauthn.TimeoutConfig{Timeout: ceremonyTimeout, TimeoutUVD: ceremonyTimeout, Enforce: true},
		},
	})
}

// storeChallenge records a ceremony and returns its random token.
func (s *Service) storeChallenge(kind, userID string, session *webauthn.SessionData) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("passkey: cannot generate challenge token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	s.challenges[token] = &challengeEntry{
		session: *session,
		kind:    kind,
		userID:  userID,
		expires: time.Now().Add(challengeTTL),
	}
	s.mu.Unlock()
	return token, nil
}

// takeChallenge removes and returns a live challenge, enforcing kind and TTL.
func (s *Service) takeChallenge(token, kind string) (*challengeEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.challenges[token]
	if !ok {
		return nil, fmt.Errorf("passkey: unknown or expired ceremony")
	}
	delete(s.challenges, token)
	if e.kind != kind {
		return nil, fmt.Errorf("passkey: ceremony kind mismatch")
	}
	if time.Now().After(e.expires) {
		return nil, fmt.Errorf("passkey: ceremony expired")
	}
	return e, nil
}

// user is the webauthn.User implementation backed by a users row.
type user struct {
	id          []byte
	name        string
	displayName string
	credentials []webauthn.Credential
}

func (u *user) WebAuthnID() []byte                         { return u.id }
func (u *user) WebAuthnName() string                       { return u.name }
func (u *user) WebAuthnDisplayName() string                { return u.displayName }
func (u *user) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// credentialRow mirrors webauthn_credentials.
type credentialRow struct {
	id              string
	userID          string
	credentialID    string // base64url raw ID
	publicKey       []byte
	attestationType string
	transports      sql.NullString // JSON array
	backupEligible  bool
	backupState     bool
	label           sql.NullString
	signCount       uint32
	createdAt       sql.NullInt64
	lastUsedAt      sql.NullInt64
}

func scanCredential(scan func(...any) error) (*credentialRow, error) {
	var c credentialRow
	var backupEligible, backupState int64
	var updatedAt sql.NullInt64
	err := scan(&c.id, &c.userID, &c.credentialID, &c.publicKey, &c.attestationType,
		&c.transports, &backupEligible, &backupState, &c.label, &c.signCount,
		&c.createdAt, &updatedAt, &c.lastUsedAt)
	if err != nil {
		return nil, err
	}
	c.backupEligible = backupEligible != 0
	c.backupState = backupState != 0
	return &c, nil
}

// toCredential converts a row to the library's credential type.
func (c *credentialRow) toCredential() (webauthn.Credential, error) {
	rawID, err := base64.RawURLEncoding.DecodeString(c.credentialID)
	if err != nil {
		return webauthn.Credential{}, fmt.Errorf("passkey: bad credential id: %w", err)
	}
	var transports []protocol.AuthenticatorTransport
	if c.transports.Valid && c.transports.String != "" {
		var names []string
		if err := json.Unmarshal([]byte(c.transports.String), &names); err != nil {
			return webauthn.Credential{}, fmt.Errorf("passkey: bad transports: %w", err)
		}
		for _, n := range names {
			transports = append(transports, protocol.AuthenticatorTransport(n))
		}
	}
	return webauthn.Credential{
		ID:              rawID,
		PublicKey:       c.publicKey,
		AttestationType: c.attestationType,
		Transport:       transports,
		Flags: webauthn.CredentialFlags{
			BackupEligible: c.backupEligible,
			BackupState:    c.backupState,
		},
		Authenticator: webauthn.Authenticator{SignCount: c.signCount},
	}, nil
}

// loadUser builds the webauthn.User for a users row (by id), with all of
// its registered credentials.
func (s *Service) loadUser(userID string) (*user, error) {
	var name, sub sql.NullString
	err := s.db.QueryRow(`SELECT name, sub FROM users WHERE id = ?`, userID).Scan(&name, &sub)
	if err != nil {
		return nil, fmt.Errorf("passkey: cannot load user: %w", err)
	}
	display := sub.String
	if name.Valid && name.String != "" {
		display = name.String
	}
	creds, err := s.loadCredentials(userID)
	if err != nil {
		return nil, err
	}
	wcreds := make([]webauthn.Credential, 0, len(creds))
	for _, c := range creds {
		wc, err := c.toCredential()
		if err != nil {
			return nil, err
		}
		wcreds = append(wcreds, wc)
	}
	return &user{
		id:          []byte(userID),
		name:        sub.String,
		displayName: display,
		credentials: wcreds,
	}, nil
}

// loadCredentials returns all credential rows for a user.
func (s *Service) loadCredentials(userID string) ([]*credentialRow, error) {
	rows, err := s.db.Query(`SELECT id, user_id, credential_id, public_key, attestation_type,
		transports, backup_eligible, backup_state, label, sign_count, created_at, updated_at, last_used_at
		FROM webauthn_credentials WHERE user_id = ? ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("passkey: cannot list credentials: %w", err)
	}
	defer rows.Close()
	var out []*credentialRow
	for rows.Next() {
		c, err := scanCredential(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("passkey: cannot scan credential: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// BeginRegistration starts a registration ceremony for userID. It returns
// the ceremony token (to echo back at finish) and the creation options for
// navigator.credentials.create().
func (s *Service) BeginRegistration(userID string, r *http.Request) (string, *protocol.CredentialCreation, error) {
	w, err := s.webAuthnFor(r)
	if err != nil {
		return "", nil, err
	}
	u, err := s.loadUser(userID)
	if err != nil {
		return "", nil, err
	}
	requireResidentKey := true
	creation, session, err := w.BeginRegistration(u,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			// No AuthenticatorAttachment: any authenticator (platform,
			// roaming, or a synced provider like LastPass) may be used.
			// residentKey "required" + requireResidentKey make the
			// credential discoverable (passkey).
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: &requireResidentKey,
			UserVerification:   protocol.VerificationPreferred,
		}),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
		webauthn.WithPublicKeyCredentialHints([]protocol.PublicKeyCredentialHints{
			protocol.PublicKeyCredentialHintHybrid,
		}),
	)
	if err != nil {
		return "", nil, fmt.Errorf("passkey: cannot begin registration: %w", err)
	}
	token, err := s.storeChallenge("registration", userID, session)
	if err != nil {
		return "", nil, err
	}
	return token, creation, nil
}

// FinishRegistration completes a registration ceremony. r's body must be the
// JSON from navigator.credentials.create(). The credential is stored with
// the given label.
func (s *Service) FinishRegistration(token, label string, r *http.Request) (*CredentialInfo, error) {
	e, err := s.takeChallenge(token, "registration")
	if err != nil {
		return nil, err
	}
	w, err := s.webAuthnFor(r)
	if err != nil {
		return nil, err
	}
	u, err := s.loadUser(e.userID)
	if err != nil {
		return nil, err
	}
	cred, err := w.FinishRegistration(u, e.session, r)
	if err != nil {
		return nil, fmt.Errorf("passkey: registration failed: %w", err)
	}

	credentialID := base64.RawURLEncoding.EncodeToString(cred.ID)
	transportNames := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transportNames = append(transportNames, string(t))
	}
	// Preserve the "hybrid" transport when the client reported it: it is
	// what enables the QR cross-device flow on machines without the
	// passkey provider installed.
	transportsJSON, _ := json.Marshal(transportNames)
	now := time.Now().Unix()
	id := newCredentialID()
	_, err = s.db.Exec(`INSERT INTO webauthn_credentials
		(id, user_id, credential_id, public_key, attestation_type, transports,
		 backup_eligible, backup_state, label, sign_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, e.userID, credentialID, cred.PublicKey, cred.AttestationType,
		string(transportsJSON), boolToInt(cred.Flags.BackupEligible),
		boolToInt(cred.Flags.BackupState), nullableLabel(label),
		cred.Authenticator.SignCount, now, now)
	if err != nil {
		return nil, fmt.Errorf("passkey: cannot store credential: %w", err)
	}
	return &CredentialInfo{
		ID:             credentialID,
		Label:          label,
		Transports:     transportNames,
		BackupEligible: cred.Flags.BackupEligible,
		BackupState:    cred.Flags.BackupState,
		CreatedAt:      now,
	}, nil
}

// BeginLogin starts a discoverable (usernameless) authentication ceremony:
// allowCredentials stays empty and hints ["hybrid"] so the browser offers
// the phone QR flow on machines without a local passkey provider.
func (s *Service) BeginLogin(r *http.Request) (string, *protocol.CredentialAssertion, error) {
	w, err := s.webAuthnFor(r)
	if err != nil {
		return "", nil, err
	}
	assertion, session, err := w.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationPreferred),
		webauthn.WithAssertionPublicKeyCredentialHints([]protocol.PublicKeyCredentialHints{
			protocol.PublicKeyCredentialHintHybrid,
		}),
	)
	if err != nil {
		return "", nil, fmt.Errorf("passkey: cannot begin login: %w", err)
	}
	token, err := s.storeChallenge("login", "", session)
	if err != nil {
		return "", nil, err
	}
	return token, assertion, nil
}

// FinishLogin completes an authentication ceremony. r's body must be the
// JSON from navigator.credentials.get(). It returns the headplane user id
// and credential id of the verified passkey.
func (s *Service) FinishLogin(token string, r *http.Request) (string, string, error) {
	e, err := s.takeChallenge(token, "login")
	if err != nil {
		return "", "", err
	}
	w, err := s.webAuthnFor(r)
	if err != nil {
		return "", "", err
	}
	cred, err := w.FinishDiscoverableLogin(s.discoverableUser, e.session, r)
	if err != nil {
		return "", "", fmt.Errorf("passkey: authentication failed: %w", err)
	}
	credentialID := base64.RawURLEncoding.EncodeToString(cred.ID)
	var userID string
	err = s.db.QueryRow(`SELECT user_id FROM webauthn_credentials WHERE credential_id = ?`, credentialID).Scan(&userID)
	if err != nil {
		return "", "", fmt.Errorf("passkey: credential has no owner: %w", err)
	}
	now := time.Now().Unix()
	_, err = s.db.Exec(`UPDATE webauthn_credentials SET sign_count = ?, last_used_at = ?, updated_at = ?
		WHERE credential_id = ?`, cred.Authenticator.SignCount, now, now, credentialID)
	if err != nil {
		s.logger.Warn("cannot update credential sign count", "error", err)
	}
	return userID, credentialID, nil
}

// discoverableUser resolves (rawID, userHandle) to the owning user for
// FinishDiscoverableLogin.
func (s *Service) discoverableUser(rawID, userHandle []byte) (webauthn.User, error) {
	credentialID := base64.RawURLEncoding.EncodeToString(rawID)
	var userID string
	err := s.db.QueryRow(`SELECT user_id FROM webauthn_credentials WHERE credential_id = ?`, credentialID).Scan(&userID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("passkey: unknown credential")
	}
	if err != nil {
		return nil, fmt.Errorf("passkey: cannot resolve credential: %w", err)
	}
	u, err := s.loadUser(userID)
	if err != nil {
		return nil, err
	}
	// The user handle binds the ceremony to the account the credential was
	// registered under.
	if len(userHandle) > 0 && string(userHandle) != userID {
		return nil, fmt.Errorf("passkey: user handle mismatch")
	}
	return u, nil
}

// List returns the registered passkeys for a user.
func (s *Service) List(userID string) ([]CredentialInfo, error) {
	rows, err := s.loadCredentials(userID)
	if err != nil {
		return nil, err
	}
	out := make([]CredentialInfo, 0, len(rows))
	for _, c := range rows {
		var transports []string
		if c.transports.Valid && c.transports.String != "" {
			_ = json.Unmarshal([]byte(c.transports.String), &transports)
		}
		label := ""
		if c.label.Valid {
			label = c.label.String
		}
		var lastUsed *int64
		if c.lastUsedAt.Valid {
			v := c.lastUsedAt.Int64
			lastUsed = &v
		}
		var created int64
		if c.createdAt.Valid {
			created = c.createdAt.Int64
		}
		out = append(out, CredentialInfo{
			ID:             c.credentialID,
			Label:          label,
			Transports:     transports,
			BackupEligible: c.backupEligible,
			BackupState:    c.backupState,
			CreatedAt:      created,
			LastUsedAt:     lastUsed,
		})
	}
	return out, nil
}

// HasAny reports whether the user has at least one registered passkey.
func (s *Service) HasAny(userID string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = ?`, userID).Scan(&n)
	return n > 0
}

// UpdateLabel renames a passkey belonging to the user.
func (s *Service) UpdateLabel(userID, credentialID, label string) error {
	res, err := s.db.Exec(`UPDATE webauthn_credentials SET label = ?, updated_at = ?
		WHERE user_id = ? AND credential_id = ?`, nullableLabel(label), time.Now().Unix(), userID, credentialID)
	if err != nil {
		return fmt.Errorf("passkey: cannot update label: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("passkey: credential not found")
	}
	return nil
}

// Delete removes a passkey belonging to the user.
func (s *Service) Delete(userID, credentialID string) error {
	res, err := s.db.Exec(`DELETE FROM webauthn_credentials WHERE user_id = ? AND credential_id = ?`,
		userID, credentialID)
	if err != nil {
		return fmt.Errorf("passkey: cannot delete credential: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("passkey: credential not found")
	}
	return nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func nullableLabel(label string) sql.NullString {
	if strings.TrimSpace(label) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: label, Valid: true}
}

// newCredentialID mints a random id for a credential row (uniqueness is
// what matters; the public identifier is credential_id).
func newCredentialID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("passkey: no randomness: " + err.Error())
	}
	return fmt.Sprintf("%x", b)
}
