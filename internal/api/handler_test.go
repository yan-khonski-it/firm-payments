package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"firm-payments/internal/payments"
)

// fakePaymentService stands in for the Postgres-backed service in handler tests.
type fakePaymentService struct{}

func (fakePaymentService) CreatePayments(context.Context, payments.Batch) error { return nil }

const sampleRequest = `{
	"payer_firm_uuid": "3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41",
	"payments": [{
		"amount": "1200.75",
		"payee_firm_uuid": "8b2e4c71-0d3a-4f6e-b1c9-5a7d2e9f4c10",
		"description": "Bookkeeping cleanup, 3 clients"
	}]
}`

// Temporary: replaced by real request tests once validation and the service exist.
func TestCreatePaymentsNotImplementedYet(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(sampleRequest))
	rec := httptest.NewRecorder()

	NewRouter(fakePaymentService{}).ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusNotImplemented, codeNotImplemented, "")
}

func TestWriteError(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{name: "without field", field: ""},
		{name: "with field", field: "payments[3].amount"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()

			writeError(rec, http.StatusBadRequest, "invalid_amount", "amount must be positive", tt.field)

			assertErrorResponse(t, rec, http.StatusBadRequest, "invalid_amount", tt.field)
		})
	}
}

func TestPaymentsRejectsOtherMethods(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/payments", nil)
	rec := httptest.NewRecorder()

	NewRouter(fakePaymentService{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// assertErrorResponse checks the status and the wire format of an error response:
// {"error": {"code": ..., "message": ..., "field": ...}}, with "field" present only when set.
func assertErrorResponse(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode, wantField string) {
	t.Helper()

	if rec.Code != wantStatus {
		t.Errorf("status = %d, want %d", rec.Code, wantStatus)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error object: %v; body = %s", err, rec.Body.String())
	}
	errObj, ok := body["error"]
	if len(body) != 1 || !ok || errObj == nil {
		t.Fatalf(`body = %s, want a single top-level "error" object`, rec.Body.String())
	}

	if errObj["code"] != wantCode {
		t.Errorf("error.code = %v, want %q", errObj["code"], wantCode)
	}
	if msg, ok := errObj["message"].(string); !ok || msg == "" {
		t.Errorf("error.message = %v, want a non-empty string", errObj["message"])
	}
	field, hasField := errObj["field"]
	switch {
	case wantField == "" && hasField:
		t.Errorf("error.field = %v, want it omitted", field)
	case wantField != "" && field != wantField:
		t.Errorf("error.field = %v, want %q", field, wantField)
	}
	for key := range errObj {
		switch key {
		case "code", "message", "field":
		default:
			t.Errorf("unexpected key error.%s", key)
		}
	}
}
