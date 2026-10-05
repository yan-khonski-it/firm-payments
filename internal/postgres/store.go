// Package postgres executes bulk payments against PostgreSQL.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"firm-payments/internal/payments"
)

// Store executes bulk payments in PostgreSQL. It keeps no state besides the
// connection pool, so any number of service instances can share one database:
// all coordination happens through row locks.
type Store struct {
	db *sql.DB
}

// NewStore returns a Store that uses db.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

type firm struct {
	id           int64
	balanceCents int64
}

// maxAttempts bounds how many times a transaction that PostgreSQL aborted is
// run in total.
const maxAttempts = 3

// CreatePayments applies a validated batch in one transaction:
//
//  1. lock every affected firm row, in ascending id order;
//  2. check that all firms exist and that the payer can cover the whole batch;
//  3. debit the payer once and credit each payee once with its summed amount;
//  4. insert one payments row per batch entry.
//
// Any error rolls the transaction back: either all balance changes and all
// payment rows are committed, or none are. The one exception is
// payments.ErrOutcomeUnknown, returned when COMMIT was sent but no answer came.
//
// A transaction that PostgreSQL aborted (see isRetryable) is run again from
// BEGIN, up to maxAttempts times. With ordered locking, deadlocks between
// payment transactions should not happen, so this is a safety net.
func (s *Store) CreatePayments(ctx context.Context, batch payments.Batch) error {
	total, err := batch.TotalCents()
	if err != nil {
		return err
	}

	for attempt := 1; ; attempt++ {
		err := s.createPaymentsOnce(ctx, batch, total)
		if err == nil || attempt == maxAttempts || !isRetryable(err) {
			return err
		}
		if sleep(ctx, retryDelay(attempt)) != nil {
			return err // the request ended; nothing was committed
		}
	}
}

func (s *Store) createPaymentsOnce(ctx context.Context, batch payments.Batch, total int64) error {
	// READ COMMITTED is PostgreSQL's default, set explicitly because the design
	// relies on it: after waiting for a row lock, SELECT ... FOR UPDATE returns
	// the latest committed balance.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// Rolls back on every early return; a no-op after a successful Commit.
	defer tx.Rollback()

	firms, err := lockFirms(ctx, tx, firmUUIDs(batch))
	if err != nil {
		return err
	}

	payer, ok := firms[batch.PayerFirmUUID]
	if !ok {
		return &payments.FirmNotFoundError{FirmUUID: batch.PayerFirmUUID, IsPayer: true}
	}
	credits := make(map[int64]int64) // payee firm id -> summed amount
	for _, p := range batch.Payments {
		payee, ok := firms[p.PayeeFirmUUID]
		if !ok {
			return &payments.FirmNotFoundError{FirmUUID: p.PayeeFirmUUID}
		}
		credits[payee.id] += p.AmountCents
	}

	if payer.balanceCents < total {
		return fmt.Errorf("%w: balance %d cents, batch total %d cents",
			payments.ErrInsufficientFunds, payer.balanceCents, total)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE firms SET balance_cents = balance_cents - $1 WHERE id = $2`,
		total, payer.id); err != nil {
		return fmt.Errorf("debit payer: %w", err)
	}
	if err := creditPayees(ctx, tx, credits); err != nil {
		return err
	}
	if err := insertPayments(ctx, tx, payer.id, batch.Payments, firms); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		if commitDefinitelyFailed(err) {
			return fmt.Errorf("commit: %w", err)
		}
		return fmt.Errorf("%w: commit: %w", payments.ErrOutcomeUnknown, err)
	}
	return nil
}

// commitDefinitelyFailed reports whether a failed Commit is known not to have
// committed anything:
//   - a context error or sql.ErrTxDone: database/sql checks the context before
//     sending COMMIT and rolls back instead, so COMMIT was never sent. Once it
//     is sent, lib/pq does not interrupt it, so a request deadline cannot leave
//     COMMIT half-way;
//   - a server error or pq.ErrInFailedTransaction: PostgreSQL answered COMMIT
//     by rolling back.
//
// Anything else, such as a dropped connection, leaves the outcome unknown.
func commitDefinitelyFailed(err error) bool {
	var pqErr *pq.Error
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, sql.ErrTxDone) ||
		errors.Is(err, pq.ErrInFailedTransaction) ||
		errors.As(err, &pqErr)
}

// isRetryable reports errors after which PostgreSQL has definitely aborted the
// transaction, so running the whole transaction again cannot apply the batch
// twice: deadlock_detected (40P01) and serialization_failure (40001, only
// possible under stricter isolation levels).
//
// Everything else is returned as is: an unknown commit outcome may already have
// paid, and a lock timeout (55P03) means contention that a retry within the
// same request would mostly wait out again.
func isRetryable(err error) bool {
	if errors.Is(err, payments.ErrOutcomeUnknown) {
		return false
	}
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && (pqErr.Code == "40P01" || pqErr.Code == "40001")
}

// retryDelay returns a jittered backoff: about 20ms before the second
// attempt and 40ms before the third.
func retryDelay(attempt int) time.Duration {
	base := 20 * time.Millisecond << (attempt - 1)
	return base/2 + rand.N(base)
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// firmUUIDs returns the payer and payee UUIDs without duplicates.
func firmUUIDs(batch payments.Batch) []string {
	seen := map[string]bool{batch.PayerFirmUUID: true}
	uuids := []string{batch.PayerFirmUUID}
	for _, p := range batch.Payments {
		if !seen[p.PayeeFirmUUID] {
			seen[p.PayeeFirmUUID] = true
			uuids = append(uuids, p.PayeeFirmUUID)
		}
	}
	return uuids
}

// lockFirmsSQL locks the rows in ascending id order. Every payment transaction
// takes its locks in this same global order, so two transactions touching the
// same firms (A -> B and B -> A) wait for each other instead of deadlocking.
const lockFirmsSQL = `
SELECT id, uuid, balance_cents
FROM firms
WHERE uuid = ANY($1)
ORDER BY id
FOR UPDATE`

// lockFirms locks the firms with the given UUIDs and returns them by UUID.
// UUIDs without a matching row are absent from the result.
func lockFirms(ctx context.Context, tx *sql.Tx, uuids []string) (map[string]firm, error) {
	rows, err := tx.QueryContext(ctx, lockFirmsSQL, pq.Array(uuids))
	if err != nil {
		return nil, fmt.Errorf("lock firms: %w", err)
	}
	defer rows.Close()

	firms := make(map[string]firm, len(uuids))
	for rows.Next() {
		var (
			f    firm
			uuid string
		)
		if err := rows.Scan(&f.id, &uuid, &f.balanceCents); err != nil {
			return nil, fmt.Errorf("lock firms: %w", err)
		}
		firms[uuid] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock firms: %w", err)
	}
	return firms, nil
}

// creditPayees adds each payee's summed amount to its balance, one UPDATE per
// payee, in ascending id order.
func creditPayees(ctx context.Context, tx *sql.Tx, credits map[int64]int64) error {
	ids := make([]int64, 0, len(credits))
	for id := range credits {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		_, err := tx.ExecContext(ctx,
			`UPDATE firms SET balance_cents = balance_cents + $1 WHERE id = $2`,
			credits[id], id)
		if isIntegerOutOfRange(err) {
			return fmt.Errorf("%w: firm id %d", payments.ErrBalanceLimitExceeded, id)
		}
		if err != nil {
			return fmt.Errorf("credit payee: %w", err)
		}
	}
	return nil
}

// insertPayments inserts one row per batch entry, in batch order, with a single
// multi-row INSERT. Entries to the same payee stay separate rows.
func insertPayments(ctx context.Context, tx *sql.Tx, payerID int64, entries []payments.Payment, firms map[string]firm) error {
	var query strings.Builder
	query.WriteString(`INSERT INTO payments (payer_firm_id, payee_firm_id, amount_cents, description) VALUES `)
	args := make([]any, 0, 4*len(entries))
	for i, p := range entries {
		if i > 0 {
			query.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&query, "($%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4)
		args = append(args, payerID, firms[p.PayeeFirmUUID].id, p.AmountCents, p.Description)
	}

	if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
		return fmt.Errorf("insert payments: %w", err)
	}
	return nil
}

// isIntegerOutOfRange reports PostgreSQL error 22003 (numeric_value_out_of_range).
func isIntegerOutOfRange(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "22003"
}
