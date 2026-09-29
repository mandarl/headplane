package auth

import (
	"database/sql"
	"fmt"
	"time"
)

// FindOrCreateUser mirrors the TS findOrCreateUser (app/server/web/auth.ts):
// look up the user by OIDC subject; if found, refresh the profile fields and
// last_login_at; otherwise insert a new "member" row and promote the very
// first user in the table to "owner" (with the matching capability bitmasks
// the TS writes to the deprecated caps column). Timestamps are whole
// seconds, matching drizzle's sqlite timestamp mode (see cookie.go).
func (s *Service) FindOrCreateUser(subject string, name, email, picture *string) (string, error) {
	now := time.Now().Unix()
	var id string
	err := s.db.QueryRow(`SELECT id FROM users WHERE sub = ? LIMIT 1`, subject).Scan(&id)
	switch {
	case err == nil:
		_, err = s.db.Exec(`UPDATE users SET name = ?, email = ?, picture = ?, last_login_at = ?, updated_at = ? WHERE id = ?`,
			nullable(name), nullable(email), nullable(picture), now, now, id)
		if err != nil {
			return "", fmt.Errorf("auth: cannot update user: %w", err)
		}
		return id, nil
	case err != sql.ErrNoRows:
		return "", fmt.Errorf("auth: cannot look up user: %w", err)
	}

	id = NewULID()
	_, err = s.db.Exec(`INSERT INTO users (id, sub, name, email, picture, role, caps, created_at, updated_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, 'member', ?, ?, ?, ?)`,
		id, subject, nullable(name), nullable(email), nullable(picture),
		int64(CapsForRole("member")), now, now, now)
	if err != nil {
		return "", fmt.Errorf("auth: cannot create user: %w", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM users`).Scan(&count); err != nil {
		return "", fmt.Errorf("auth: cannot count users: %w", err)
	}
	if count == 1 {
		_, err = s.db.Exec(`UPDATE users SET role = 'owner', caps = ?, updated_at = ? WHERE id = ?`,
			int64(CapsForRole("owner")), now, id)
		if err != nil {
			return "", fmt.Errorf("auth: cannot promote first user: %w", err)
		}
	}
	return id, nil
}

// LinkHeadscaleUser mirrors the TS linkHeadscaleUser: it refuses to steal a
// Headscale user id that is already claimed by a different headplane user,
// otherwise it records the link. It returns false when the link was refused.
func (s *Service) LinkHeadscaleUser(userID, headscaleUserID string) (bool, error) {
	var existingID string
	err := s.db.QueryRow(`SELECT id FROM users WHERE headscale_user_id = ? LIMIT 1`, headscaleUserID).Scan(&existingID)
	switch {
	case err == nil:
		if existingID != userID {
			return false, nil
		}
		return true, nil
	case err != sql.ErrNoRows:
		return false, fmt.Errorf("auth: cannot check headscale user link: %w", err)
	}
	_, err = s.db.Exec(`UPDATE users SET headscale_user_id = ?, updated_at = ? WHERE id = ?`,
		headscaleUserID, time.Now().Unix(), userID)
	if err != nil {
		return false, fmt.Errorf("auth: cannot link headscale user: %w", err)
	}
	return true, nil
}

func nullable(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// FindOrCreatePasskeyUser returns the users row that backs passkey
// registration for an API-key principal. API-key sessions have no users
// row, so one is derived from the key itself (stable per key via its
// hash). API-key holders bypass every role check, so the derived user is
// created as 'owner' to preserve capability parity with the API-key
// session — a 'member' would silently lose access the key grants.
func (s *Service) FindOrCreatePasskeyUser(apiKey, displayName string) (string, error) {
	subject := "passkey:api-key:" + HashAPIKey(apiKey)
	now := time.Now().Unix()
	var id string
	err := s.db.QueryRow(`SELECT id FROM users WHERE sub = ? LIMIT 1`, subject).Scan(&id)
	switch {
	case err == nil:
		_, err = s.db.Exec(`UPDATE users SET name = ?, last_login_at = ?, updated_at = ? WHERE id = ?`,
			nullable(orNilStr(displayName)), now, now, id)
		if err != nil {
			return "", fmt.Errorf("auth: cannot update passkey user: %w", err)
		}
		return id, nil
	case err != sql.ErrNoRows:
		return "", fmt.Errorf("auth: cannot look up passkey user: %w", err)
	}

	id = NewULID()
	_, err = s.db.Exec(`INSERT INTO users (id, sub, name, role, caps, created_at, updated_at, last_login_at)
		VALUES (?, ?, ?, 'owner', ?, ?, ?, ?)`,
		id, subject, nullable(orNilStr(displayName)), int64(CapsForRole("owner")), now, now, now)
	if err != nil {
		return "", fmt.Errorf("auth: cannot create passkey user: %w", err)
	}
	return id, nil
}

// RecordLogin refreshes last_login_at for the user, mirroring what
// FindOrCreateUser does on OIDC login.
func (s *Service) RecordLogin(userID string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`UPDATE users SET last_login_at = ?, updated_at = ? WHERE id = ?`, now, now, userID)
	if err != nil {
		return fmt.Errorf("auth: cannot record login: %w", err)
	}
	return nil
}

// UserProfile returns the display profile fields for a users row.
func (s *Service) UserProfile(userID string) (name string, email *string, err error) {
	var nameNS, emailNS sql.NullString
	err = s.db.QueryRow(`SELECT name, email FROM users WHERE id = ?`, userID).Scan(&nameNS, &emailNS)
	if err != nil {
		return "", nil, fmt.Errorf("auth: cannot load user profile: %w", err)
	}
	if nameNS.Valid {
		name = nameNS.String
	}
	if emailNS.Valid {
		e := emailNS.String
		email = &e
	}
	return name, email, nil
}

func orNilStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
