package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"firm-payments/internal/payments"
)

// fakePaymentService stands in for the Postgres-backed service in handler
// tests. It records each call and returns err.
type fakePaymentService struct {
	err         error
	calls       []payments.Batch
	hasDeadline bool
	deadline    time.Time
}

func (f *fakePaymentService) CreatePayments(ctx context.Context, batch payments.Batch) error {
	f.calls = append(f.calls, batch)
	f.deadline, f.hasDeadline = ctx.Deadline()
	return f.err
}

const sampleRequest = `{
	"payer_firm_uuid": "3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41",
	"payments": [{
		"amount": "1200.75",
		"payee_firm_uuid": "8b2e4c71-0d3a-4f6e-b1c9-5a7d2e9f4c10",
		"description": "Bookkeeping cleanup, 3 clients"
	}]
}`

func TestCreatePaymentsSuccess(t *testing.T) {
	svc := &fakePaymentService{}
	req := newRequest(http.MethodPost, "/payments", sampleRequest)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	start := time.Now()

	rec := serveWith(svc, req)
	end := time.Now()

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	wantBody := map[string]any{
		"payer_firm_uuid": "3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41",
		"payment_count":   float64(1),
		"total_amount":    "1200.75",
	}
	if !reflect.DeepEqual(body, wantBody) {
		t.Errorf("body = %v, want %v", body, wantBody)
	}

	wantBatch := []payments.Batch{{
		PayerFirmUUID: "3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41",
		Payments: []payments.Payment{{
			PayeeFirmUUID: "8b2e4c71-0d3a-4f6e-b1c9-5a7d2e9f4c10",
			AmountCents:   120075,
			Description:   "Bookkeeping cleanup, 3 clients",
		}},
	}}
	if !reflect.DeepEqual(svc.calls, wantBatch) {
		t.Errorf("service calls = %+v, want %+v", svc.calls, wantBatch)
	}
	// The deadline is requestTimeout after the handler started.
	if !svc.hasDeadline || svc.deadline.Before(start.Add(requestTimeout)) || svc.deadline.After(end.Add(requestTimeout)) {
		t.Errorf("service context deadline = %v (set: %v), want %v after the request", svc.deadline, svc.hasDeadline, requestTimeout)
	}
}

func TestCreatePaymentsInvalidRequestDoesNotCallService(t *testing.T) {
	svc := &fakePaymentService{}

	rec := serveWith(svc, newRequest(http.MethodPost, "/payments", `{"payments":[]}`))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if len(svc.calls) != 0 {
		t.Errorf("service called %d times, want 0", len(svc.calls))
	}
}

func TestCreatePaymentsServiceErrors(t *testing.T) {
	const (
		payer  = "3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41"
		lopez  = "8b2e4c71-0d3a-4f6e-b1c9-5a7d2e9f4c10"
		nair   = "e5f18b3c-2a9d-4c07-8e6b-1d4a7f9c3b25"
		secret = "pq: password authentication failed for user app"
	)
	body := `{"payer_firm_uuid":"` + payer + `","payments":[
		{"amount":"1","payee_firm_uuid":"` + lopez + `","description":""},
		{"amount":"2","payee_firm_uuid":"` + nair + `","description":""},
		{"amount":"3","payee_firm_uuid":"` + nair + `","description":""}]}`

	tests := []struct {
		name           string
		err            error
		wantStatus     int
		wantCode       string
		wantField      string
		wantRetryAfter bool
	}{
		{"payer not found", &payments.FirmNotFoundError{FirmUUID: payer, IsPayer: true},
			http.StatusNotFound, codePayerNotFound, "payer_firm_uuid", false},
		{"payee not found", &payments.FirmNotFoundError{FirmUUID: nair},
			http.StatusNotFound, codePayeeNotFound, "payments[1].payee_firm_uuid", false},
		{"insufficient funds", fmt.Errorf("%w: balance 1, total 6", payments.ErrInsufficientFunds),
			http.StatusUnprocessableEntity, codeInsufficientFunds, "", false},
		{"balance limit exceeded", payments.ErrBalanceLimitExceeded,
			http.StatusUnprocessableEntity, codeBalanceLimitExceeded, "", false},
		{"busy", fmt.Errorf("%w: lock timeout", payments.ErrBusy),
			http.StatusServiceUnavailable, codeBusy, "", true},
		{"deadlock retries exhausted", fmt.Errorf("%w: %w", payments.ErrBusy, errors.New("pq: deadlock detected (40P01)")),
			http.StatusServiceUnavailable, codeBusy, "", true},
		{"request deadline", fmt.Errorf("lock firms: %w", context.DeadlineExceeded),
			http.StatusServiceUnavailable, codeTimeout, "", true},
		{"deadline during retry backoff", fmt.Errorf("%w (retry interrupted after: %w)", context.DeadlineExceeded, errors.New("deadlock")),
			http.StatusServiceUnavailable, codeTimeout, "", true},
		{"outcome unknown", fmt.Errorf("%w: commit: unexpected EOF", payments.ErrOutcomeUnknown),
			http.StatusInternalServerError, codeOutcomeUnknown, "", false},
		{"anything else", errors.New(secret),
			http.StatusInternalServerError, codeInternal, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveWith(&fakePaymentService{err: tt.err}, newRequest(http.MethodPost, "/payments", body))

			assertErrorResponse(t, rec, tt.wantStatus, tt.wantCode, tt.wantField)
			if got := rec.Header().Get("Retry-After") != ""; got != tt.wantRetryAfter {
				t.Errorf("Retry-After present = %v, want %v", got, tt.wantRetryAfter)
			}
			if strings.Contains(rec.Body.String(), "password") {
				t.Errorf("body leaks the internal error: %s", rec.Body.String())
			}
		})
	}
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
	methods := []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			rec := serve(newRequest(method, "/payments", ""))

			assertErrorResponse(t, rec, http.StatusMethodNotAllowed, codeMethodNotAllowed, "")
			if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
				t.Errorf("Allow = %q, want %q", allow, http.MethodPost)
			}
		})
	}
}

func TestUnknownPathsReturnNotFound(t *testing.T) {
	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/payments/"},
		{method: http.MethodPost, path: "/payments/123"},
		{method: http.MethodPost, path: "/unknown"},
		{method: http.MethodGet, path: "/"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := serve(newRequest(tt.method, tt.path, sampleRequest))

			assertErrorResponse(t, rec, http.StatusNotFound, codeNotFound, "")
		})
	}
}

// newRequest builds a request; a non-empty body is sent as JSON.
func newRequest(method, target, body string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func serve(req *http.Request) *httptest.ResponseRecorder {
	return serveWith(&fakePaymentService{}, req)
}

func serveWith(svc PaymentService, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	NewRouter(svc).ServeHTTP(rec, req)
	return rec
}

// assertErrorResponse checks the status and the wire format of an error response:
// {"error": {"code": ..., "message": ..., "field": ...}}, with "field" present only when set.
func assertErrorResponse(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode, wantField string) {
	t.Helper()

	if rec.Code != wantStatus {
		t.Errorf("status = %d, want %d", rec.Code, wantStatus)
	}
	contentType := rec.Header().Get("Content-Type")
	if mediaType, _, err := mime.ParseMediaType(contentType); err != nil || mediaType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
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
