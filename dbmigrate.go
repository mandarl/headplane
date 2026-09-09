// Package dbmigrate applies Headplane's drizzle SQL migrations to the
// SQLite database. The Node server runs these via drizzle-orm's migrator on
// every boot (app/server/db/client.server.ts); the Go server must apply the
// same migrations so a fresh data directory gets the full schema (users,
// auth_sessions, host_info, ...) and upgrades from older versions work.
//
// This mirrors drizzle-orm's SQLite migrator protocol exactly so the two
// servers can share one database file in either order:
//   - tracking table __drizzle_migrations(id, hash, created_at, name,
//     applied_at), created with the same DDL;
//   - an already-applied migration is detected by folder name;
//   - the recorded row carries sha256(migration.sql), the folder timestamp
//     as UTC millis, the folder name, and an ISO-8601 applied_at.
//
// A database the Node server already migrated is therefore a no-op here,
// and vice versa.
//
// The drizzle/meta/_journal.json is not committed, so migration order is
// the lexicographic order of the timestamped directory names, which matches
// drizzle's folderMillis ordering (all names share a 14-digit timestamp
// prefix). This lives in a root-level package because go:embed patterns
// cannot escape the package directory with "..".
package dbmigrate

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed drizzle
var drizzleFS embed.FS

// migrationsTableDDL is drizzle-orm's own CREATE TABLE for the tracking
// table (sqlite-core/dialect.ts, SQLiteSyncDialect.migrate).
const migrationsTableDDL = `CREATE TABLE IF NOT EXISTS __drizzle_migrations (
	id INTEGER PRIMARY KEY,
	hash text NOT NULL,
	created_at numeric,
	name text,
	applied_at TEXT
)`

// Migrate applies any pending drizzle migrations to db.
func Migrate(db *sql.DB) error {
	if _, err := db.Exec(migrationsTableDDL); err != nil {
		return fmt.Errorf("dbmigrate: cannot create migrations table: %w", err)
	}
	// Upgrade path for pre-"name"-column tables (drizzle's v0 -> v1).
	if err := ensureColumns(db); err != nil {
		return err
	}

	applied := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM __drizzle_migrations`)
	if err != nil {
		return fmt.Errorf("dbmigrate: cannot read migrations table: %w", err)
	}
	for rows.Next() {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if name.Valid {
			applied[name.String] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	entries, err := drizzleFS.ReadDir("drizzle")
	if err != nil {
		return fmt.Errorf("dbmigrate: cannot read embedded drizzle migrations: %w", err)
	}
	names := []string{}
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if applied[name] {
			continue
		}
		raw, err := drizzleFS.ReadFile("drizzle/" + name + "/migration.sql")
		if err != nil {
			return fmt.Errorf("dbmigrate: cannot read migration %s: %w", name, err)
		}
		sum := sha256.Sum256(raw)
		if err := applyMigration(db, string(raw)); err != nil {
			return fmt.Errorf("dbmigrate: migration %s failed: %w", name, err)
		}
		if _, err := db.Exec(
			`INSERT INTO __drizzle_migrations (hash, created_at, name, applied_at) VALUES (?, ?, ?, ?)`,
			fmt.Sprintf("%x", sum), folderMillis(name), name,
			time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("dbmigrate: cannot record migration %s: %w", name, err)
		}
	}
	return nil
}

// ensureColumns adds the v1 columns (name, applied_at) when the tracking
// table predates them, mirroring drizzle's upgradeSyncIfNeeded.
func ensureColumns(db *sql.DB) error {
	cols := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('__drizzle_migrations')`)
	if err != nil {
		return fmt.Errorf("dbmigrate: cannot inspect migrations table: %w", err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return err
		}
		cols[c] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if !cols["name"] {
		if _, err := db.Exec(`ALTER TABLE __drizzle_migrations ADD COLUMN name text`); err != nil {
			return fmt.Errorf("dbmigrate: cannot add name column: %w", err)
		}
	}
	if !cols["applied_at"] {
		if _, err := db.Exec(`ALTER TABLE __drizzle_migrations ADD COLUMN applied_at TEXT`); err != nil {
			return fmt.Errorf("dbmigrate: cannot add applied_at column: %w", err)
		}
	}
	return nil
}

// folderMillis parses the 14-digit timestamp prefix of a migration folder
// name as UTC millis, like drizzle's formatToMillis.
func folderMillis(name string) int64 {
	if len(name) < 14 {
		return 0
	}
	d := name[:14]
	num := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	t := time.Date(num(d[0:4]), time.Month(num(d[4:6])), num(d[6:8]),
		num(d[8:10]), num(d[10:12]), num(d[12:14]), 0, time.UTC)
	return t.UnixMilli()
}

// applyMigration runs one migration.sql: statements are separated by
// drizzle's `--> statement-breakpoint` markers (which also appear trailing
// on the same line as a statement).
func applyMigration(db *sql.DB, raw string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Normalize: put every breakpoint marker on its own line, then split.
	normalized := strings.ReplaceAll(raw, "--> statement-breakpoint", "\n--> statement-breakpoint\n")
	for _, part := range strings.Split(normalized, "--> statement-breakpoint") {
		lines := []string{}
		for _, ln := range strings.Split(part, "\n") {
			if t := strings.TrimSpace(ln); !strings.HasPrefix(t, "--") {
				lines = append(lines, ln)
			}
		}
		stmt := strings.TrimSpace(strings.Join(lines, "\n"))
		stmt = strings.TrimSuffix(stmt, ";")
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("statement %q: %w", truncate(stmt, 80), err)
		}
	}
	return tx.Commit()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
