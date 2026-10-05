package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/lib/pq"

	"firm-payments/internal/payments"
)

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"deadlock", &pq.Error{Code: "40P01"}, true},
		{"serialization failure", &pq.Error{Code: "40001"}, true},
		{"wrapped deadlock", fmt.Errorf("lock firms: %w", &pq.Error{Code: "40P01"}), true},
		{"lock timeout", &pq.Error{Code: "55P03"}, false},
		{"integer out of range", &pq.Error{Code: "22003"}, false},
		{"insufficient funds", payments.ErrInsufficientFunds, false},
		{"connection error", io.ErrUnexpectedEOF, false},
		{"unknown commit outcome", fmt.Errorf("%w: %w", payments.ErrOutcomeUnknown, &pq.Error{Code: "40P01"}), false},
	}
	for _, tt := range tests {
		if got := isRetryable(tt.err); got != tt.want {
			t.Errorf("%s: isRetryable(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}

func TestCommitDefinitelyFailed(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"context cancelled before COMMIT", context.Canceled, true},
		{"deadline passed before COMMIT", context.DeadlineExceeded, true},
		{"already rolled back", sql.ErrTxDone, true},
		{"server rejected COMMIT", &pq.Error{Code: "40001"}, true},
		{"transaction had failed", pq.ErrInFailedTransaction, true},
		{"connection dropped", io.ErrUnexpectedEOF, false},
		{"bad connection", driver.ErrBadConn, false},
	}
	for _, tt := range tests {
		if got := commitDefinitelyFailed(tt.err); got != tt.want {
			t.Errorf("%s: commitDefinitelyFailed(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}

// Forces a real deadlock between CreatePayments and another transaction that
// locks the same rows in the opposite order. PostgreSQL aborts CreatePayments'
// transaction (it waited first); CreatePayments must run it again from BEGIN
// and apply the batch exactly once.
func TestCreatePaymentsRetriesAfterDeadlock(t *testing.T) {
	db := openTestDB(t)
	ctx := testContext(t)

	// The other transaction locks Nair (id 3) first.
	other, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer other.Rollback()
	if _, err := other.ExecContext(ctx, `SELECT 1 FROM firms WHERE id = $1 FOR UPDATE`, nairID); err != nil {
		t.Fatalf("lock Nair: %v", err)
	}

	// CreatePayments locks Pinecrest (id 1), then waits for Nair.
	done := make(chan error, 1)
	go func() {
		done <- NewStore(db).CreatePayments(ctx, batch(pinecrestUUID, pay(nairUUID, 100, "retried")))
	}()
	waitForLockWaiters(t, db, 1)

	// Now the other transaction wants Pinecrest: a cycle. Its SELECT returns
	// once PostgreSQL aborts CreatePayments' transaction as the deadlock victim.
	if _, err := other.ExecContext(ctx, `SELECT 1 FROM firms WHERE id = $1 FOR UPDATE`, pinecrestID); err != nil {
		t.Fatalf("lock Pinecrest: %v", err)
	}
	// The retry is now waiting for the other transaction; release its locks.
	if err := other.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// ctx's deadline bounds this wait: CreatePayments returns when it expires.
	if err := <-done; err != nil {
		t.Fatalf("CreatePayments: %v", err)
	}

	want := map[string]int64{pinecrestUUID: 5000000 - 100, lopezUUID: 50000, nairUUID: 200000 + 100}
	if got := balances(t, db); !reflect.DeepEqual(got, want) {
		t.Errorf("balances = %v, want %v", got, want)
	}
	if rows := paymentRows(t, db); len(rows) != 1 {
		t.Errorf("payments rows = %v, want exactly one", rows)
	}
}

// waitForLockWaiters waits until n sessions are blocked on a row lock.
func waitForLockWaiters(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		err := db.QueryRow(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		if err != nil {
			t.Fatalf("query pg_stat_activity: %v", err)
		}
		if waiting >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d lock waiter(s)", n)
}
