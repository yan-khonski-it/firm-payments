// Package payments defines the domain types for bulk payments, shared by the
// HTTP layer and the storage layer.
package payments

// Batch is a validated bulk payment: one payer paying one or more payees.
type Batch struct {
	PayerFirmUUID string
	Payments      []Payment
}

// Payment is one entry of a Batch. Every entry becomes its own row in the
// payments table, even when several entries go to the same payee.
type Payment struct {
	PayeeFirmUUID string
	// AmountCents is int64 so summing a batch cannot overflow; validation caps
	// each amount to fit the INTEGER amount_cents column.
	AmountCents int64
	Description string
}
