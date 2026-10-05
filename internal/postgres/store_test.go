package postgres

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"

	"firm-payments/internal/payments"
	"firm-payments/internal/testdb"
)

// Firms from sql/000003_seed_firms.up.sql.
const (
	pinecrestUUID = "3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41" // id 1, $50,000.00
	lopezUUID     = "8b2e4c71-0d3a-4f6e-b1c9-5a7d2e9f4c10" // id 2, $500.00
	nairUUID      = "e5f18b3c-2a9d-4c07-8e6b-1d4a7f9c3b25" // id 3, $2,000.00
	unknownUUID   = "00000000-0000-4000-8000-000000000000"

	pinecrestID = 1
	lopezID     = 2
	nairID      = 3
)

var seedBalances = map[string]int64{pinecrestUUID: 5000000, lopezUUID: 50000, nairUUID: 200000}

// testTimeout bounds each test, so a transaction that waits on a lock forever
// fails the test promptly instead of hanging until go test's own timeout.
const testTimeout = 10 * time.Second

// openTestDB resets the shared test database to the seed data and returns it
// with the test's context; see testdb.Open.
func openTestDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	return testdb.Open(t, testTimeout)
}

func balances(ctx context.Context, t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT uuid, balance_cents FROM firms`)
	if err != nil {
		t.Fatalf("query balances: %v", err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var uuid string
		var balance int64
		if err := rows.Scan(&uuid, &balance); err != nil {
			t.Fatalf("scan balance: %v", err)
		}
		got[uuid] = balance
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read balances: %v", err)
	}
	return got
}

type paymentRow struct {
	PayerID, PayeeID int64
	AmountCents      int64
	Description      string
}

func paymentRows(ctx context.Context, t *testing.T, db *sql.DB) []paymentRow {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT payer_firm_id, payee_firm_id, amount_cents, description FROM payments ORDER BY id`)
	if err != nil {
		t.Fatalf("query payments: %v", err)
	}
	defer rows.Close()
	var got []paymentRow
	for rows.Next() {
		var r paymentRow
		if err := rows.Scan(&r.PayerID, &r.PayeeID, &r.AmountCents, &r.Description); err != nil {
			t.Fatalf("scan payment: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read payments: %v", err)
	}
	return got
}

// assertUnchanged checks that a failed batch left no trace.
func assertUnchanged(ctx context.Context, t *testing.T, db *sql.DB, wantBalances map[string]int64) {
	t.Helper()
	if got := balances(ctx, t, db); !reflect.DeepEqual(got, wantBalances) {
		t.Errorf("balances = %v, want unchanged %v", got, wantBalances)
	}
	if rows := paymentRows(ctx, t, db); len(rows) != 0 {
		t.Errorf("payments rows = %v, want none", rows)
	}
}

func batch(payer string, entries ...payments.Payment) payments.Batch {
	return payments.Batch{PayerFirmUUID: payer, Payments: entries}
}

func pay(payee string, cents int64, description string) payments.Payment {
	return payments.Payment{PayeeFirmUUID: payee, AmountCents: cents, Description: description}
}

func TestCreatePaymentsAggregatesDuplicatePayees(t *testing.T) {
	db, ctx := openTestDB(t)
	store := NewStore(db)

	err := store.CreatePayments(ctx, batch(pinecrestUUID,
		pay(lopezUUID, 10000, "Invoice 1"),
		pay(lopezUUID, 20000, "Invoice 2"),
		pay(nairUUID, 5000, ""),
	))
	if err != nil {
		t.Fatalf("CreatePayments: %v", err)
	}

	wantBalances := map[string]int64{
		pinecrestUUID: 5000000 - 35000,
		lopezUUID:     50000 + 30000,
		nairUUID:      200000 + 5000,
	}
	if got := balances(ctx, t, db); !reflect.DeepEqual(got, wantBalances) {
		t.Errorf("balances = %v, want %v", got, wantBalances)
	}
	wantRows := []paymentRow{
		{pinecrestID, lopezID, 10000, "Invoice 1"},
		{pinecrestID, lopezID, 20000, "Invoice 2"},
		{pinecrestID, nairID, 5000, ""},
	}
	if got := paymentRows(ctx, t, db); !reflect.DeepEqual(got, wantRows) {
		t.Errorf("payments rows = %v, want %v", got, wantRows)
	}
}

func TestCreatePaymentsExactBalance(t *testing.T) {
	db, ctx := openTestDB(t)

	err := NewStore(db).CreatePayments(ctx, batch(lopezUUID,
		pay(nairUUID, 20000, "a"),
		pay(pinecrestUUID, 30000, "b"),
	))
	if err != nil {
		t.Fatalf("CreatePayments: %v", err)
	}
	if got := balances(ctx, t, db)[lopezUUID]; got != 0 {
		t.Errorf("payer balance = %d, want 0", got)
	}
}

func TestCreatePaymentsInsufficientFunds(t *testing.T) {
	db, ctx := openTestDB(t)

	// Each entry fits Lopez's $500 on its own; the batch total does not.
	err := NewStore(db).CreatePayments(ctx, batch(lopezUUID,
		pay(nairUUID, 30000, "a"),
		pay(nairUUID, 20001, "b"),
	))
	if !errors.Is(err, payments.ErrInsufficientFunds) {
		t.Fatalf("err = %v, want %v", err, payments.ErrInsufficientFunds)
	}
	assertUnchanged(ctx, t, db, seedBalances)
}

func TestCreatePaymentsFirmNotFound(t *testing.T) {
	tests := []struct {
		name  string
		batch payments.Batch
		want  payments.FirmNotFoundError
	}{
		{
			name:  "payer",
			batch: batch(unknownUUID, pay(lopezUUID, 100, "")),
			want:  payments.FirmNotFoundError{FirmUUID: unknownUUID, IsPayer: true},
		},
		{
			name:  "payee",
			batch: batch(pinecrestUUID, pay(lopezUUID, 100, ""), pay(unknownUUID, 100, "")),
			want:  payments.FirmNotFoundError{FirmUUID: unknownUUID},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, ctx := openTestDB(t)

			err := NewStore(db).CreatePayments(ctx, tt.batch)

			var notFound *payments.FirmNotFoundError
			if !errors.As(err, &notFound) || *notFound != tt.want {
				t.Fatalf("err = %v, want %+v", err, tt.want)
			}
			assertUnchanged(ctx, t, db, seedBalances)
		})
	}
}

// A failure after earlier writes in the same transaction must undo them all.
func TestCreatePaymentsRollsBackOnFailure(t *testing.T) {
	db, ctx := openTestDB(t)
	if _, err := db.ExecContext(ctx, `UPDATE firms SET balance_cents = $1 WHERE id = $2`, math.MaxInt32-100, nairID); err != nil {
		t.Fatalf("set up balance: %v", err)
	}
	before := balances(ctx, t, db)

	// The payer debit and Lopez's credit succeed; Nair's credit overflows INTEGER.
	err := NewStore(db).CreatePayments(ctx, batch(pinecrestUUID,
		pay(lopezUUID, 1000, "ok"),
		pay(nairUUID, 1000, "overflows"),
	))
	if !errors.Is(err, payments.ErrBalanceLimitExceeded) {
		t.Fatalf("err = %v, want %v", err, payments.ErrBalanceLimitExceeded)
	}
	assertUnchanged(ctx, t, db, before)
}

// A failure in the last write stage, after the payer debit and the payee
// credits, must undo them too. The batch bypasses HTTP validation, so the
// database's own description length check rejects the INSERT.
func TestCreatePaymentsRollsBackWhenInsertFails(t *testing.T) {
	db, ctx := openTestDB(t)

	err := NewStore(db).CreatePayments(ctx, batch(pinecrestUUID,
		pay(lopezUUID, 1000, "ok"),
		pay(nairUUID, 1000, strings.Repeat("x", 501)),
	))

	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || pqErr.Constraint != "payments_description_max_length" {
		t.Fatalf("err = %v, want a payments_description_max_length violation", err)
	}
	assertUnchanged(ctx, t, db, seedBalances)
}

// Ten copies of a $13,251.25 batch against Pinecrest's $50,000: exactly three
// fit, whichever order the transactions run in.
func TestCreatePaymentsConcurrentBatchesFromOnePayer(t *testing.T) {
	db, ctx := openTestDB(t)
	store := NewStore(db)
	b := batch(pinecrestUUID,
		pay(lopezUUID, 120075, "Bookkeeping cleanup, 3 clients"),
		pay(nairUUID, 30000, "Referral fee, 2 clients"),
		pay(nairUUID, 580050, "Tax prep overflow"),
		pay(nairUUID, 595000, "Audit support"),
	)

	errs := runConcurrently(10, func(int) error { return store.CreatePayments(ctx, b) })

	var succeeded, insufficient int
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, payments.ErrInsufficientFunds):
			insufficient++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != 3 || insufficient != 7 {
		t.Errorf("succeeded = %d, insufficient = %d; want 3 and 7", succeeded, insufficient)
	}

	want := map[string]int64{pinecrestUUID: 1024625, lopezUUID: 410225, nairUUID: 3815150}
	if got := balances(ctx, t, db); !reflect.DeepEqual(got, want) {
		t.Errorf("balances = %v, want %v", got, want)
	}
	if rows := paymentRows(ctx, t, db); len(rows) != 3*len(b.Payments) {
		t.Errorf("payments rows = %d, want %d", len(rows), 3*len(b.Payments))
	}
}

// Transactions paying in opposite directions (A -> B and B -> A) would
// deadlock with payer-first locking. With ascending-id locking no deadlock may
// happen at all: CreatePayments retries deadlocks, so a passing payment alone
// would hide a lock-order regression; the test therefore also requires zero
// retries. Every payment must succeed and money must be conserved.
func TestCreatePaymentsOpposingDirectionsDoNotDeadlock(t *testing.T) {
	db, ctx := openTestDB(t)
	store := NewStore(db)
	var retries atomic.Int32
	store.onRetry = func(error) { retries.Add(1) }
	batches := []payments.Batch{
		batch(pinecrestUUID, pay(lopezUUID, 1, "")),
		batch(lopezUUID, pay(pinecrestUUID, 1, "")),
		batch(nairUUID, pay(lopezUUID, 1, ""), pay(pinecrestUUID, 1, "")),
		batch(lopezUUID, pay(nairUUID, 1, ""), pay(pinecrestUUID, 1, "")),
		batch(pinecrestUUID, pay(nairUUID, 1, ""), pay(lopezUUID, 1, "")),
	}

	const workers, iterations = 20, 25
	errs := runConcurrently(workers, func(w int) error {
		for i := 0; i < iterations; i++ {
			if err := store.CreatePayments(ctx, batches[(w+i)%len(batches)]); err != nil {
				return err
			}
		}
		return nil
	})
	for _, err := range errs {
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if n := retries.Load(); n != 0 {
		t.Errorf("retries = %d, want 0: deadlocks happened, so the lock order is broken", n)
	}

	var total int64
	for _, b := range balances(ctx, t, db) {
		total += b
	}
	if want := int64(5000000 + 50000 + 200000); total != want {
		t.Errorf("total balance = %d, want %d (money must be conserved)", total, want)
	}
}

// runConcurrently starts n goroutines at once and returns their errors.
func runConcurrently(n int, fn func(worker int) error) []error {
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			errs[w] = fn(w)
		}(w)
	}
	close(start)
	wg.Wait()
	return errs
}
