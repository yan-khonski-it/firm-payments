package postgres

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"firm-payments/internal/payments"
)

func TestIsTimeout(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"lock timeout", &pq.Error{Code: "55P03"}, true},
		{"statement timeout", fmt.Errorf("credit payee: %w", &pq.Error{Code: "57014"}), true},
		{"deadlock", &pq.Error{Code: "40P01"}, false},
		{"connection error", io.ErrUnexpectedEOF, false},
		{"unknown commit outcome", fmt.Errorf("%w: %w", payments.ErrOutcomeUnknown, &pq.Error{Code: "57014"}), false},
	}
	for _, tt := range tests {
		if got := isTimeout(tt.err); got != tt.want {
			t.Errorf("%s: isTimeout(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}

// A payee locked by another transaction makes CreatePayments give up after
// lock_timeout with ErrBusy, without retrying and without changing anything.
func TestCreatePaymentsLockTimeout(t *testing.T) {
	db, ctx := openTestDB(t)
	store := NewStore(db)
	store.lockTimeout = 200 * time.Millisecond
	retries := 0
	store.onRetry = func(error) { retries++ }

	other, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer other.Rollback()
	if _, err := other.ExecContext(ctx, `SELECT 1 FROM firms WHERE id = $1 FOR UPDATE`, nairID); err != nil {
		t.Fatalf("lock Nair: %v", err)
	}

	start := time.Now()
	err = store.CreatePayments(ctx, batch(pinecrestUUID, pay(lopezUUID, 100, ""), pay(nairUUID, 100, "")))
	elapsed := time.Since(start)

	if !errors.Is(err, payments.ErrBusy) {
		t.Fatalf("err = %v, want %v", err, payments.ErrBusy)
	}
	if elapsed > 2*time.Second {
		t.Errorf("CreatePayments took %v, want about the 200ms lock timeout", elapsed)
	}
	if retries != 0 {
		t.Errorf("retries = %d, want 0", retries)
	}

	if err := other.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	assertUnchanged(ctx, t, db, seedBalances)
}

// The timeouts are SET LOCAL: once the transaction ends, the pooled connection
// is back to its previous settings.
func TestCreatePaymentsTimeoutsDoNotLeakToPooledConnection(t *testing.T) {
	db, ctx := openTestDB(t)
	db.SetMaxOpenConns(1) // the checks must reuse the payment's connection

	settings := func() map[string]string {
		got := map[string]string{}
		for _, name := range []string{"lock_timeout", "statement_timeout", "idle_in_transaction_session_timeout"} {
			var value string
			if err := db.QueryRowContext(ctx, "SHOW "+name).Scan(&value); err != nil {
				t.Fatalf("show %s: %v", name, err)
			}
			got[name] = value
		}
		return got
	}

	before := settings()
	if err := NewStore(db).CreatePayments(ctx, batch(pinecrestUUID, pay(lopezUUID, 100, ""))); err != nil {
		t.Fatalf("CreatePayments: %v", err)
	}
	if after := settings(); !reflect.DeepEqual(after, before) {
		t.Errorf("settings after the payment = %v, want %v as before", after, before)
	}
}

// A real statement timeout after earlier writes: another transaction holds a
// table lock on payments, so the INSERT blocks after the payer debit and the
// payee credits have run. PostgreSQL cancels it (57014); CreatePayments must
// return ErrBusy without retrying and leave nothing behind.
func TestCreatePaymentsStatementTimeoutRollsBack(t *testing.T) {
	db, ctx := openTestDB(t)
	store := NewStore(db)
	store.statementTimeout = 200 * time.Millisecond
	store.lockTimeout = 5 * time.Second // longer, so the statement timeout fires first
	retries := 0
	store.onRetry = func(error) { retries++ }

	other, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer other.Rollback()
	if _, err := other.ExecContext(ctx, `LOCK TABLE payments IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock payments: %v", err)
	}

	err = store.CreatePayments(ctx, batch(pinecrestUUID, pay(lopezUUID, 100, ""), pay(nairUUID, 100, "")))

	var pqErr *pq.Error
	if !errors.Is(err, payments.ErrBusy) || !errors.As(err, &pqErr) || pqErr.Code != "57014" {
		t.Fatalf("err = %v, want ErrBusy wrapping a statement timeout (57014)", err)
	}
	if !strings.Contains(err.Error(), "insert payments") {
		t.Errorf("err = %v, want the timeout in the INSERT, after the balance updates", err)
	}
	if retries != 0 {
		t.Errorf("retries = %d, want 0", retries)
	}

	if err := other.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	assertUnchanged(ctx, t, db, seedBalances)
}
