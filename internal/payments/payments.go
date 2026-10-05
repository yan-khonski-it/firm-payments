// Package payments defines the domain types for bulk payments, shared by the
// HTTP layer and the storage layer.
package payments

import (
	"errors"
	"math"
)

// ErrTotalOverflow is returned when a batch total does not fit in int64.
var ErrTotalOverflow = errors.New("payments: batch total overflows int64")

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
