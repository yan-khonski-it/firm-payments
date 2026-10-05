package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync/atomic"
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
		{"statement timeout", &pq.Error{Code: "57014"}, false},
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

func TestRetryAborted(t *testing.T) {
	deadlock := &pq.Error{Code: "40P01"}
	serialization := &pq.Error{Code: "40001"}
	lockTimeout := &pq.Error{Code: "55P03"}
	statementTimeout := &pq.Error{Code: "57014"}
	unknown := fmt.Errorf("%w: commit: %w", payments.ErrOutcomeUnknown, io.ErrUnexpectedEOF)

	tests := []struct {
		name        string
		results     []error // what op returns on each call
		wantCalls   int
		wantRetries int
		wantErr     error
	}{
		{"success", []error{nil}, 1, 0, nil},
		{"deadlock, then success", []error{deadlock, nil}, 2, 1, nil},
		{"serialization failure, then success", []error{serialization, nil}, 2, 1, nil},
		{"aborted every time", []error{deadlock, deadlock, deadlock}, maxAttempts, maxAttempts - 1, deadlock},
		{"lock timeout is not retried", []error{lockTimeout}, 1, 0, lockTimeout},
		{"statement timeout is not retried", []error{statementTimeout}, 1, 0, statementTimeout},
		{"unknown outcome is not retried", []error{unknown}, 1, 0, payments.ErrOutcomeUnknown},
		{"business error is not retried", []error{payments.ErrInsufficientFunds}, 1, 0, payments.ErrInsufficientFunds},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls, retries := 0, 0
			err := retryAborted(context.Background(), func(error) { retries++ }, func() error {
				calls++
				return tt.results[calls-1]
			})

			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if calls != tt.wantCalls || retries != tt.wantRetries {
				t.Errorf("calls = %d, retries = %d; want %d and %d", calls, retries, tt.wantCalls, tt.wantRetries)
			}
		})
	}
}

// When the request ends during the backoff, the caller must be able to tell
// that from the error, as well as what the last attempt failed with.
func TestRetryAbortedStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryAborted(ctx, nil, func() error {
		calls++
		cancel()
		return &pq.Error{Code: "40P01"}
	})

	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to match context.Canceled", err)
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || pqErr.Code != "40P01" {
		t.Errorf("err = %v, want it to wrap the deadlock error", err)
	}
}

// Forces a real deadlock between CreatePayments and another transaction that
// locks the same rows in the opposite order. PostgreSQL does not promise which
// transaction it aborts, so both outcomes are accepted:
//   - CreatePayments is the victim: it must retry once and succeed;
//   - the other transaction is the victim: CreatePayments succeeds without a retry.
//
// Either way the batch must be applied exactly once.
func TestCreatePaymentsDeadlockWithAnotherTransaction(t *testing.T) {
	db, ctx := openTestDB(t)
	store := NewStore(db)
	var retries atomic.Int32
	store.onRetry = func(error) { retries.Add(1) }

	// The other transaction locks Nair (id 3) first.
	other, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer other.Rollback()
	if _, err := other.ExecContext(ctx, `SELECT 1 FROM firms WHERE id = $1 FOR UPDATE`, nairID); err != nil {
		t.Fatalf("lock Nair: %v", err)
	}
	var otherPID int
	if err := other.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&otherPID); err != nil {
		t.Fatalf("read the other transaction's process id: %v", err)
	}

	// CreatePayments locks Pinecrest (id 1), then waits for Nair.
	done := make(chan error, 1)
	go func() {
		done <- store.CreatePayments(ctx, batch(pinecrestUUID, pay(nairUUID, 100, "deadlocked")))
	}()
	waitUntilBlockedBy(ctx, t, db, otherPID)

	// Now the other transaction wants Pinecrest: a cycle, which PostgreSQL
	// breaks by aborting one of the two.
	_, otherErr := other.ExecContext(ctx, `SELECT 1 FROM firms WHERE id = $1 FOR UPDATE`, pinecrestID)
	otherWasVictim := isDeadlock(otherErr)
	if otherErr != nil && !otherWasVictim {
		t.Fatalf("lock Pinecrest: %v", otherErr)
	}
	// Release the other transaction's locks, so CreatePayments can finish.
	_ = other.Rollback()

	if err := <-done; err != nil { // ctx's deadline bounds this wait
		t.Fatalf("CreatePayments: %v", err)
	}

	wantRetries := int32(1)
	if otherWasVictim {
		wantRetries = 0
	}
	t.Logf("deadlock victim: other transaction = %v; retries = %d", otherWasVictim, retries.Load())
	if got := retries.Load(); got != wantRetries {
		t.Errorf("retries = %d, want %d", got, wantRetries)
	}

	want := map[string]int64{pinecrestUUID: 5000000 - 100, lopezUUID: 50000, nairUUID: 200000 + 100}
	if got := balances(ctx, t, db); !reflect.DeepEqual(got, want) {
		t.Errorf("balances = %v, want %v", got, want)
	}
	if rows := paymentRows(ctx, t, db); len(rows) != 1 {
		t.Errorf("payments rows = %v, want exactly one", rows)
	}
}

func isDeadlock(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "40P01"
}

// waitUntilBlockedBy waits until some session is waiting for a lock held by
// the session with process id blocker. Other sessions waiting for unrelated
// locks, such as another package's test waiting for the shared test database,
// do not count. Database tests run one at a time, so the blocked session is
// the one under test.
func waitUntilBlockedBy(ctx context.Context, t *testing.T, db *sql.DB, blocker int) {
	t.Helper()
	for {
		var blocked bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked)
		if err != nil {
			t.Fatalf("waiting for a session blocked by process %d: %v", blocker, err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
