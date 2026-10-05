package api

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
)

var errDelivery = errors.New("connection reset by peer")

// brokenWriter is a ResponseWriter whose client has gone away: either the
// write fails, or the write is buffered and the flush fails.
type brokenWriter struct {
	header        http.Header
	failOnWrite   bool
	failOnFlush   bool
	statusWritten int
}

func (b *brokenWriter) Header() http.Header {
	if b.header == nil {
		b.header = http.Header{}
	}
	return b.header
}

func (b *brokenWriter) WriteHeader(status int) { b.statusWritten = status }

func (b *brokenWriter) Write(p []byte) (int, error) {
	if b.failOnWrite {
		return 0, errDelivery
	}
	return len(p), nil
}

// FlushError is what http.ResponseController.Flush calls.
func (b *brokenWriter) FlushError() error {
	if b.failOnFlush {
		return errDelivery
	}
	return nil
}

func TestWriteJSONReportsDeliveryFailure(t *testing.T) {
	tests := []struct {
		name string
		w    *brokenWriter
	}{
		{"write fails", &brokenWriter{failOnWrite: true}},
		{"flush fails", &brokenWriter{failOnFlush: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := writeJSON(tt.w, http.StatusCreated, map[string]int{"a": 1}); !errors.Is(err, errDelivery) {
				t.Errorf("writeJSON error = %v, want %v", err, errDelivery)
			}
		})
	}
	if err := writeJSON(&brokenWriter{}, http.StatusCreated, map[string]int{"a": 1}); err != nil {
		t.Errorf("writeJSON on a working writer = %v, want nil", err)
	}
}

// When the 201 cannot be delivered after the payments committed, the handler
// logs it and does not run the batch again.
func TestCreatePaymentsLogsUndeliveredConfirmation(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	svc := &fakePaymentService{}
	NewRouter(svc).ServeHTTP(&brokenWriter{failOnFlush: true}, newRequest(http.MethodPost, "/payments", sampleRequest))

	if len(svc.calls) != 1 {
		t.Errorf("service called %d times, want exactly 1", len(svc.calls))
	}
	if !strings.Contains(logs.String(), "payments committed for payer 3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41 (1 payments, 1200.75) but the 201 response was not delivered") {
		t.Errorf("log = %q, want the undelivered confirmation logged", logs.String())
	}
}
