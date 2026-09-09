package auth

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ServiceOverride mirrors getServiceOverrides in
// app/server/service-overrides.ts: the Headplane-side description override
// for one `${proto}:${port}` service, with the timestamp as an ISO string
// (nil when unset) so it flows straight into loader data.
type ServiceOverride struct {
	Description string
	UpdatedBy   *string
	UpdatedAt   *string
}

// GetServiceOverrides returns all description overrides for a node keyed by
// `${proto}:${port}`, mirroring getServiceOverrides.
func (s *Service) GetServiceOverrides(hostID string) (map[string]ServiceOverride, error) {
	rows, err := s.db.Query(
		`SELECT proto, port, description, updated_by, updated_at
		 FROM service_description_overrides WHERE host_id = ?`, hostID)
	if err != nil {
		return nil, fmt.Errorf("auth: get service overrides: %w", err)
	}
	defer rows.Close()

	out := map[string]ServiceOverride{}
	for rows.Next() {
		var proto string
		var port int64
		var description string
		var updatedBy sql.NullString
		var updatedAt sql.NullInt64
		if err := rows.Scan(&proto, &port, &description, &updatedBy, &updatedAt); err != nil {
			return nil, fmt.Errorf("auth: scan service override: %w", err)
		}
		ov := ServiceOverride{Description: description}
		if updatedBy.Valid {
			v := updatedBy.String
			ov.UpdatedBy = &v
		}
		if updatedAt.Valid {
			v := time.UnixMilli(updatedAt.Int64).UTC().Format("2006-01-02T15:04:05.000Z07:00")
			ov.UpdatedAt = &v
		}
		out[fmt.Sprintf("%s:%d", proto, port)] = ov
	}
	return out, rows.Err()
}

// SetServiceOverride mirrors setServiceOverride: an empty/whitespace-only
// description deletes the override, otherwise it upserts it. The row id is
// `${hostID}:${proto}:${port}` like the TS synthetic key.
func (s *Service) SetServiceOverride(hostID, proto string, port int64, description string, updatedBy *string) error {
	// Mirrors setServiceOverride: the description is trimmed before the
	// empty check, so whitespace-only input deletes the row.
	description = strings.TrimSpace(description)
	id := fmt.Sprintf("%s:%s:%d", hostID, proto, port)
	if description == "" {
		_, err := s.db.Exec(`DELETE FROM service_description_overrides WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("auth: delete service override: %w", err)
		}
		return nil
	}
	var ub sql.NullString
	if updatedBy != nil {
		ub = sql.NullString{String: *updatedBy, Valid: true}
	}
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(
		`INSERT INTO service_description_overrides
		 (id, host_id, proto, port, description, updated_by, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		 description = excluded.description,
		 updated_by = excluded.updated_by,
		 updated_at = excluded.updated_at`,
		id, hostID, proto, port, description, ub, now)
	if err != nil {
		return fmt.Errorf("auth: set service override: %w", err)
	}
	return nil
}

// UpsertHostInfo stores one agent sync's hostinfo payloads, mirroring the
// hostInfo upsert in hp-agent.ts. Payloads are raw JSON; updated_at is
// epoch millis like drizzle's timestamp mode.
func (s *Service) UpsertHostInfo(payloads map[string]json.RawMessage) error {
	if len(payloads) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("auth: upsert host info: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for hostID, payload := range payloads {
		if _, err := tx.Exec(
			`INSERT INTO host_info (host_id, payload, updated_at)
			 VALUES (?, ?, ?)
			 ON CONFLICT(host_id) DO UPDATE SET
			 payload = excluded.payload,
			 updated_at = excluded.updated_at`,
			hostID, string(payload), now); err != nil {
			return fmt.Errorf("auth: upsert host info: %w", err)
		}
	}
	return tx.Commit()
}

// LookupHostInfo returns the stored payloads for the given node keys,
// mirroring AgentManager.lookup (rows without a payload are skipped).
func (s *Service) LookupHostInfo(nodeKeys []string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(nodeKeys) == 0 {
		return out, nil
	}
	placeholders := ""
	args := make([]any, 0, len(nodeKeys))
	for i, k := range nodeKeys {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, k)
	}
	rows, err := s.db.Query(
		`SELECT host_id, payload FROM host_info WHERE host_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("auth: lookup host info: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hostID string
		var payload sql.NullString
		if err := rows.Scan(&hostID, &payload); err != nil {
			return nil, fmt.Errorf("auth: scan host info: %w", err)
		}
		if !payload.Valid || payload.String == "" {
			continue
		}
		out[hostID] = json.RawMessage(payload.String)
	}
	return out, rows.Err()
}

// PruneHostInfoNotIn deletes host_info rows whose host_id is not in
// activeKeys, mirroring pruneStaleHostInfo. A no-op when activeKeys is
// empty (the TS version returns early in that case too).
func (s *Service) PruneHostInfoNotIn(activeKeys []string) (int64, error) {
	if len(activeKeys) == 0 {
		return 0, nil
	}
	placeholders := ""
	args := make([]any, 0, len(activeKeys))
	for i, k := range activeKeys {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, k)
	}
	res, err := s.db.Exec(
		`DELETE FROM host_info WHERE host_id NOT IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("auth: prune host info: %w", err)
	}
	return res.RowsAffected()
}
