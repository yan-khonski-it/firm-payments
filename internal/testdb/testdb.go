// Package testdb prepares the shared PostgreSQL test database for integration
// tests.
package testdb

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/lib/pq" // registers the "postgres" driver
)

// lockKey identifies the advisory lock that serializes tests using the test
// database. Any constant works, as long as every test uses the same one.
const lockKey = 20261005

// lockWait bounds how long a test waits for another test to release the
// database. It is separate from the test's own timeout.
const lockWait = 60 * time.Second

// Open connects to TEST_DATABASE_URL, which must point to a migrated database
// that tests may wipe, and resets it to the seed data. It skips the test when
// TEST_DATABASE_URL is unset.
//
// go test runs packages in parallel and they all share that database, so Open
// first takes a PostgreSQL advisory lock and holds it until the test ends:
// tests that use the database run one at a time.
//
// The returned context ends timeout after the lock is acquired, so waiting for
// another test does not use up this test's time; it bounds setup, the test and
// its checks.
func Open(t *testing.T, timeout time.Duration) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration tests")
	}

	lock(t, dsn)

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	t.Cleanup(cancel)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(20)

	reset(ctx, t, db)
	return db, ctx
}

// lock takes the advisory lock on a session of its own, waiting at most
// lockWait. Closing that session when the test ends releases the lock.
func lock(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), lockWait)
	defer cancel()

	lockDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open lock connection: %v", err)
	}
	conn, err := lockDB.Conn(ctx)
	if err != nil {
		lockDB.Close()
		t.Fatalf("open lock connection: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		lockDB.Close()
	})
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		t.Fatalf("waited up to %v for another test to release the test database: %v", lockWait, err)
	}
}

// reset empties both tables and re-applies the seed migration.
func reset(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	seed, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "sql", "000003_seed_firms.up.sql"))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE payments, firms RESTART IDENTITY`); err != nil {
		t.Fatalf("reset tables: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(seed)); err != nil {
		t.Fatalf("apply seed: %v", err)
	}
}
