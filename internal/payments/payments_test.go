package payments

import (
	"errors"
	"math"
	"testing"
)

func TestTotalCents(t *testing.T) {
	batch := Batch{Payments: []Payment{{AmountCents: 120075}, {AmountCents: 30000}, {AmountCents: 30000}}}
	if got, err := batch.TotalCents(); err != nil || got != 180075 {
		t.Errorf("TotalCents() = %d, %v; want 180075, nil", got, err)
	}

	// The largest batch validation allows: 1000 payments of the INTEGER maximum.
	largest := Batch{Payments: make([]Payment, 1000)}
	for i := range largest.Payments {
		largest.Payments[i].AmountCents = math.MaxInt32
	}
	if got, err := largest.TotalCents(); err != nil || got != 1000*math.MaxInt32 {
		t.Errorf("TotalCents() of largest valid batch = %d, %v; want %d, nil", got, err, int64(1000*math.MaxInt32))
	}

	// Exactly math.MaxInt64 still fits; only going past it overflows.
	boundary := Batch{Payments: []Payment{{AmountCents: math.MaxInt64 - 1}, {AmountCents: 1}}}
	if got, err := boundary.TotalCents(); err != nil || got != math.MaxInt64 {
		t.Errorf("TotalCents() at the boundary = %d, %v; want %d, nil", got, err, int64(math.MaxInt64))
	}

	overflow := Batch{Payments: []Payment{{AmountCents: math.MaxInt64}, {AmountCents: 1}}}
	if _, err := overflow.TotalCents(); !errors.Is(err, ErrTotalOverflow) {
		t.Errorf("TotalCents() on overflow: err = %v, want %v", err, ErrTotalOverflow)
	}
}
