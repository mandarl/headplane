package auth

import (
	"database/sql"
	"fmt"
	"time"
)

// HeadplaneUser is the full users-table row, as the /users page needs it.
type HeadplaneUser struct {
	ID              string
	Sub             string
	Name            *string
	Email           *string
	Picture         *string
	Role            string
	HeadscaleUserID *string
	CreatedAt       int64
	LastLoginAt     int64
	UpdatedAt       int64
}

func scanHeadplaneUser(row interface {
	Scan(dest ...any) error
},
) (HeadplaneUser, error) {
	var u HeadplaneUser
	var name, email, picture, hsID sql.NullString
	var createdAt, lastLoginAt, updatedAt sql.NullInt64
	err := row.Scan(&u.ID, &u.Sub, &name, &email, &picture, &u.Role, &hsID, &createdAt, &lastLoginAt, &updatedAt)
	if err != nil {
		return HeadplaneUser{}, err
	}
	u.Name = nullStrPtr(name)
	u.Email = nullStrPtr(email)
	u.Picture = nullStrPtr(picture)
	u.HeadscaleUserID = nullStrPtr(hsID)
	u.CreatedAt = createdAt.Int64
	u.LastLoginAt = lastLoginAt.Int64
	u.UpdatedAt = updatedAt.Int64
	return u, nil
}

func nullStrPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

// GetUserByID returns one Headplane user row.
func (s *Service) GetUserByID(userID string) (HeadplaneUser, error) {
	u, err := scanHeadplaneUser(s.db.QueryRow(
		`SELECT id, sub, name, email, picture, role, headscale_user_id, created_at, last_login_at, updated_at
		 FROM users WHERE id = ?`, userID))
	if err == sql.ErrNoRows {
		return HeadplaneUser{}, fmt.Errorf("auth: user not found")
	}
	return u, err
}

// ListUsers returns every Headplane user row, mirroring listUsers().
func (s *Service) ListUsers() ([]HeadplaneUser, error) {
	rows, err := s.db.Query(
		`SELECT id, sub, name, email, picture, role, headscale_user_id, created_at, last_login_at, updated_at
		 FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeadplaneUser
	for rows.Next() {
		u, err := scanHeadplaneUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ClaimedHeadscaleUserIds mirrors claimedHeadscaleUserIds(): the set of
// Headscale user ids already linked to a Headplane user.
func (s *Service) ClaimedHeadscaleUserIds() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT headscale_user_id FROM users WHERE headscale_user_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// UnlinkHeadscaleUser mirrors unlinkHeadscaleUser(): clears the link and
// returns false when the user row does not exist.
func (s *Service) UnlinkHeadscaleUser(userID string) (bool, error) {
	res, err := s.db.Exec(`UPDATE users SET headscale_user_id = NULL, updated_at = ? WHERE id = ?`,
		time.Now().Unix(), userID)
	if err != nil {
		return false, fmt.Errorf("auth: cannot unlink headscale user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ReassignUser mirrors reassignUser(): changes a user's role (and derived
// capability bits) and reports whether it applied. It refuses when the user
// does not exist or already holds the owner role.
func (s *Service) ReassignUser(userID, role string) (bool, error) {
	var current string
	err := s.db.QueryRow(`SELECT role FROM users WHERE id = ?`, userID).Scan(&current)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current == "owner" {
		return false, nil
	}
	res, err := s.db.Exec(`UPDATE users SET role = ?, caps = ?, updated_at = ? WHERE id = ?`,
		role, int64(CapsForRole(role)), time.Now().Unix(), userID)
	if err != nil {
		return false, fmt.Errorf("auth: cannot reassign user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// TransferOwnership mirrors transferOwnership(): the current owner steps
// down to admin and the target becomes owner. It returns false when the ids
// are equal, the caller is not the owner, or the target does not exist.
func (s *Service) TransferOwnership(currentOwnerID, newOwnerID string) (bool, error) {
	if currentOwnerID == newOwnerID {
		return false, nil
	}
	var currentRole string
	err := s.db.QueryRow(`SELECT role FROM users WHERE id = ?`, currentOwnerID).Scan(&currentRole)
	if err == sql.ErrNoRows || currentRole != "owner" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var targetID string
	if err := s.db.QueryRow(`SELECT id FROM users WHERE id = ?`, newOwnerID).Scan(&targetID); err == sql.ErrNoRows {
		return false, nil
	} else if err != nil {
		return false, err
	}

	// Both role flips run in one transaction so a crash between them can
	// never leave the tailnet with zero owners.
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err := tx.Exec(`UPDATE users SET role = 'admin', caps = ?, updated_at = ? WHERE id = ?`,
		int64(CapsForRole("admin")), now, currentOwnerID); err != nil {
		return false, fmt.Errorf("auth: cannot step down current owner: %w", err)
	}
	if _, err := tx.Exec(`UPDATE users SET role = 'owner', caps = ?, updated_at = ? WHERE id = ?`,
		int64(CapsForRole("owner")), now, newOwnerID); err != nil {
		return false, fmt.Errorf("auth: cannot promote new owner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
