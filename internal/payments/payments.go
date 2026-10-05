// Package payments defines the domain types for bulk payments, shared by the
// HTTP layer and the storage layer.
package payments

import (
	"errors"
	"fmt"
	"math"
)

var (
	// ErrTotalOverflow is returned when a batch total does not fit in int64.
	ErrTotalOverflow = errors.New("payments: batch total overflows int64")
	// ErrInsufficientFunds is returned when the payer's balance cannot cover
	// the whole batch. Nothing is changed.
	ErrInsufficientFunds = errors.New("payments: insufficient funds")
	// ErrBalanceLimitExceeded is returned when a credit would push a payee's
	// balance past what the INTEGER balance column can hold. Nothing is changed.
	ErrBalanceLimitExceeded = errors.New("payments: payee balance limit exceeded")
	// ErrOutcomeUnknown is returned when COMMIT was sent but its result never
	// arrived, for example because the connection dropped. The batch may or may
	// not have been applied, so retrying it blindly can pay twice.
	ErrOutcomeUnknown = errors.New("payments: outcome unknown")
)

// FirmNotFoundError reports a firm referenced by a batch that does not exist.
type FirmNotFoundError struct {
	FirmUUID string
	IsPayer  bool
}

func (e *FirmNotFoundError) Error() string {
	role := "payee"
	if e.IsPayer {
		role = "payer"
	}
	return fmt.Sprintf("payments: %s firm %s not found", role, e.FirmUUID)
}

// Batch is a validated bulk payment: one payer paying one or more payees.
type Batch struct {
	PayerFirmUUID string
	Payments      []Payment
}

// Payment is one entry of a Batch. Every entry becomes its own row in the
// payments table, even when several entries go to the same payee.
type Payment struct {
	PayeeFirmUUID string
	// Validated amounts and batch sizes keep totals within int64; TotalCents also checks for overflow.
	AmountCents int64
	Description string
}

// TotalCents returns the sum of all amounts in the batch. Amounts are positive
// after validation; the sum is checked so it fails instead of wrapping around.
func (b Batch) TotalCents() (int64, error) {
	var total int64
	for _, p := range b.Payments {
		if p.AmountCents > 0 && total > math.MaxInt64-p.AmountCents {
			return 0, ErrTotalOverflow
		}
		total += p.AmountCents
	}
	return total, nil
}
