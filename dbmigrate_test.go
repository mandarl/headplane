package dbmigrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func hasTable(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&n); err != nil || n != 1 {
		t.Fatalf("table %s missing: %v", table, err)
		return false
	}
	return true
}

func TestMigrateFreshAndIdempotent(t *testing.T) {
	db := openTestDB(t)

	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	// The full drizzle schema is present (ephemeral_nodes was created by
	// the first migration and dropped by a later one, like the Node
	// server's database).
	for _, table := range []string{
		"users", "auth_sessions", "host_info",
		"service_description_overrides",
		"__drizzle_migrations",
	} {
		hasTable(t, db, table)
	}

	// The tracking table uses drizzle's own shape.
	var cols string
	rows, err := db.Query(`SELECT group_concat(name, ',') FROM pragma_table_info('__drizzle_migrations')`)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	rows.Scan(&cols)
	rows.Close()
	for _, want := range []string{"id", "hash", "created_at", "name", "applied_at"} {
		found := false
		for _, c := range splitCSV(cols) {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("migrations table columns = %q, want %q", cols, want)
		}
	}

	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM __drizzle_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied == 0 {
		t.Fatal("no migrations recorded")
	}
	// Rows carry a sha256 hash and an ISO-8601 applied_at, like drizzle's.
	var hash, appliedAt string
	if err := db.QueryRow(
		`SELECT hash, applied_at FROM __drizzle_migrations LIMIT 1`).Scan(&hash, &appliedAt); err != nil {
		t.Fatal(err)
	}
	if len(hash) != 64 {
		t.Fatalf("hash = %q, want 64 hex chars", hash)
	}
	if appliedAt == "" {
		t.Fatal("applied_at empty")
	}

	// Second run is a no-op.
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var applied2 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM __drizzle_migrations`).Scan(&applied2); err != nil {
		t.Fatal(err)
	}
	if applied2 != applied {
		t.Fatalf("applied changed: %d -> %d", applied, applied2)
	}

	// The migrated schema accepts the rows the auth service writes.
	if _, err := db.Exec(
		`INSERT INTO auth_sessions (id, kind, api_key_hash, api_key_display, expires_at, created_at)
		 VALUES ('s1', 'api_key', 'h', 'd', 9999999999, 1)`); err != nil {
		t.Fatalf("insert into auth_sessions: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO users (id, sub, role) VALUES ('u1', 'sub-1', 'owner')`); err != nil {
		t.Fatalf("insert into users: %v", err)
	}
}

// TestMigrateNodeMigratedDB simulates a database the Node server already
// migrated: Migrate must be a no-op and must not fail on drizzle's own
// tracking-table shape.
func TestMigrateNodeMigratedDB(t *testing.T) {
	db := openTestDB(t)

	// Node's drizzle migrator DDL and row shape.
	if _, err := db.Exec(`CREATE TABLE __drizzle_migrations (
		id INTEGER PRIMARY KEY,
		hash text NOT NULL,
		created_at numeric,
		name text,
		applied_at TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE users (
		id text PRIMARY KEY,
		sub text NOT NULL UNIQUE,
		role text NOT NULL DEFAULT 'member'
	)`); err != nil {
		t.Fatal(err)
	}
	entries, err := drizzleFS.ReadDir("drizzle")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := db.Exec(
			`INSERT INTO __drizzle_migrations (hash, created_at, name, applied_at)
			 VALUES ('deadbeef', 0, ?, '2026-01-01T00:00:00Z')`, e.Name()); err != nil {
			t.Fatal(err)
		}
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate on Node-migrated DB: %v", err)
	}
	// No new rows: every embedded migration was already recorded.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM __drizzle_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, e := range entries {
		if e.IsDir() {
			want++
		}
	}
	if n != want {
		t.Fatalf("rows = %d, want %d (no-op)", n, want)
	}
	// The pre-existing users table is untouched.
	hasTable(t, db, "users")
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}
