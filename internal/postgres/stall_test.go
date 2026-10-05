package postgres

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"firm-payments/internal/payments"
)

// A database that stops answering mid-transaction. lib/pq does not watch the
// request context during BEGIN, and for later statements a cancelled context
// only sends a cancel request, which a silent server never answers. The
// connection's I/O timeout must end the wait in every case:
//   - before COMMIT, nothing is paid: ErrBusy;
//   - during COMMIT, PostgreSQL may have committed: ErrOutcomeUnknown, never
//     retried.
func TestCreatePaymentsDatabaseStopsAnswering(t *testing.T) {
	const ioTimeout = 300 * time.Millisecond
	applied := map[string]int64{pinecrestUUID: 5000000 - 100, lopezUUID: 50000 + 100, nairUUID: 200000}

	tests := []struct {
		name         string
		trigger      []byte
		wantErr      error
		wantBalances map[string]int64
		wantRows     int
	}{
		{"after BEGIN", []byte("BEGIN"), payments.ErrBusy, seedBalances, 0},
		{"during the lock query", []byte("FOR UPDATE"), payments.ErrBusy, seedBalances, 0},
		{"during COMMIT", commitMessage, payments.ErrOutcomeUnknown, applied, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, ctx := openTestDB(t)
			proxy := startFaultProxy(t, tt.trigger, stall)
			stalled, err := Open(proxy.dsn, ioTimeout)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { stalled.Close() })
			store := NewStore(stalled)
			retries := 0
			store.onRetry = func(error) { retries++ }

			start := time.Now()
			err = store.CreatePayments(ctx, batch(pinecrestUUID, pay(lopezUUID, 100, "stalled")))
			elapsed := time.Since(start)

			if !proxy.triggered.Load() {
				t.Fatal("the proxy never stalled, so the test did not exercise an unresponsive database")
			}
			if !errors.Is(err, tt.wantErr) || !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("err = %v, want %v caused by the I/O timeout", err, tt.wantErr)
			}
			if elapsed > 2*time.Second {
				t.Errorf("CreatePayments took %v, want about the %v I/O timeout", elapsed, ioTimeout)
			}
			if retries != 0 {
				t.Errorf("retries = %d, want 0", retries)
			}
			if got := balances(ctx, t, db); !reflect.DeepEqual(got, tt.wantBalances) {
				t.Errorf("balances = %v, want %v", got, tt.wantBalances)
			}
			if rows := paymentRows(ctx, t, db); len(rows) != tt.wantRows {
				t.Errorf("payments rows = %d, want %d", len(rows), tt.wantRows)
			}
		})
	}
}
