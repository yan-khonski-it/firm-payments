// Package postgres executes bulk payments against PostgreSQL.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
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

	// PostgreSQL-side limits for each transaction, see setTimeouts.
	lockTimeout              time.Duration
	statementTimeout         time.Duration
	idleInTransactionTimeout time.Duration

	// onRetry, when set, is called before each retry. Tests use it to see
	// whether a retry happened.
	onRetry func(err error)
}

// Default database timeouts. They sit below the request deadline set by the
// caller, which bounds the whole operation including retries:
// lock timeout < statement timeout < request deadline < HTTP write timeout.
const (
	defaultLockTimeout              = 2 * time.Second
	defaultStatementTimeout         = 5 * time.Second
	defaultIdleInTransactionTimeout = 5 * time.Second
)

// NewStore returns a Store that uses db.
func NewStore(db *sql.DB) *Store {
	return &Store{
		db:                       db,
		lockTimeout:              defaultLockTimeout,
		statementTimeout:         defaultStatementTimeout,
		idleInTransactionTimeout: defaultIdleInTransactionTimeout,
	}
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

	err = retryAborted(ctx, s.onRetry, func() error {
		return s.createPaymentsOnce(ctx, batch, total)
	})
	return classifyBusy(ctx, err)
}

// classifyBusy labels the result of CreatePayments, in this order:
//
//  1. Success and an unknown commit outcome (including an I/O timeout during
//     COMMIT) are returned unchanged: the payments may have been applied, so
//     nothing else may be claimed about them.
//  2. A statement cancelled because the request's context ended is reported
//     as the context error. lib/pq surfaces that cancellation as PostgreSQL's
//     query_canceled (57014), which on its own looks like statement_timeout.
//     The original error stays wrapped.
//  3. Context errors are returned unchanged.
//  4. Errors meaning "temporarily unavailable, nothing changed" are wrapped
//     in payments.ErrBusy: a PostgreSQL timeout, an abort (deadlock,
//     serialization failure) that still happened on the last attempt, or a
//     database that stopped answering before COMMIT (see DefaultIOTimeout).
//
// Other errors, such as insufficient funds, are returned unchanged even if
// the context has ended meanwhile.
func classifyBusy(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, payments.ErrOutcomeUnknown) {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil && isQueryCanceled(err) {
		return fmt.Errorf("%w: %w", ctxErr, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if isTimeout(err) || isRetryable(err) || errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("%w: %w", payments.ErrBusy, err)
	}
	return err
}

// retryAborted runs op until it succeeds, fails with an error that is not
// retryable, or has run maxAttempts times, waiting a jittered backoff between
// attempts. If ctx ends during the backoff, the returned error wraps both the
// context error and the last op error, so errors.Is matches either.
func retryAborted(ctx context.Context, onRetry func(error), op func() error) error {
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || attempt == maxAttempts || !isRetryable(err) {
			return err
		}
		if ctxErr := sleep(ctx, retryDelay(attempt)); ctxErr != nil {
			return fmt.Errorf("%w (retry interrupted after: %w)", ctxErr, err)
		}
		if onRetry != nil {
			onRetry(err)
		}
	}
}

// createPaymentsOnce applies the batch in a single transaction attempt.
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

	if err := s.setTimeouts(ctx, tx); err != nil {
		return err
	}

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

// commitDefinitelyFailed reports whether Commit is known to have rolled back.
// For this single, non-concurrent Commit call, context errors and sql.ErrTxDone
// mean database/sql rolled back before sending COMMIT. PostgreSQL errors and
// pq.ErrInFailedTransaction also indicate rollback. Transport errors leave the
// outcome unknown because COMMIT may have reached the server.
func commitDefinitelyFailed(err error) bool {
	var pqErr *pq.Error
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, sql.ErrTxDone) ||
		errors.Is(err, pq.ErrInFailedTransaction) ||
		errors.As(err, &pqErr)
}

// isRetryable reports whether the entire transaction should be retried.
// Deadlocks and serialization failures abort the transaction and are safe to
// retry. An unknown commit result is not retryable because the payments may
// have been applied. Lock timeouts are not retried because the blocking
// transaction is likely still running.
func isRetryable(err error) bool {
	if errors.Is(err, payments.ErrOutcomeUnknown) {
		return false
	}
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && (pqErr.Code == "40P01" || pqErr.Code == "40001")
}

// isTimeout reports a PostgreSQL timeout: lock_not_available (55P03, from
// lock_timeout) or query_canceled (57014, from statement_timeout). lib/pq also
// returns 57014 when the request's context ends during a statement;
// classifyBusy checks the context first to tell the two apart. An unknown
// commit outcome is never reported as a timeout.
func isTimeout(err error) bool {
	if errors.Is(err, payments.ErrOutcomeUnknown) {
		return false
	}
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && (pqErr.Code == "55P03" || pqErr.Code == "57014")
}

// isQueryCanceled reports PostgreSQL's query_canceled (57014).
func isQueryCanceled(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "57014"
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

// setTimeouts limits this transaction on the PostgreSQL side, so it cannot wait
// forever even if the caller's context has no deadline:
//   - lock_timeout bounds each wait for a row lock (fails with 55P03);
//   - statement_timeout bounds each statement (fails with 57014);
//   - idle_in_transaction_session_timeout ends the session, releasing its
//     locks, if the application stalls between statements.
//
// set_config(..., true) is SET LOCAL: the values end with the transaction and
// never leak to other users of the pooled connection.
func (s *Store) setTimeouts(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT
		set_config('lock_timeout', $1, true),
		set_config('statement_timeout', $2, true),
		set_config('idle_in_transaction_session_timeout', $3, true)`,
		pgDuration(s.lockTimeout), pgDuration(s.statementTimeout), pgDuration(s.idleInTransactionTimeout))
	if err != nil {
		return fmt.Errorf("set timeouts: %w", err)
	}
	return nil
}

func pgDuration(d time.Duration) string {
	return fmt.Sprintf("%dms", d.Milliseconds())
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
