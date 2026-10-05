package api

import (
	"context"
	"encoding/json"
	"net/http"

	"firm-payments/internal/payments"
)

// PaymentService applies a validated batch of payments atomically.
// The HTTP layer depends on this interface rather than on the database,
// so handlers can be unit-tested with a fake.
type PaymentService interface {
	CreatePayments(ctx context.Context, batch payments.Batch) error
}

// errorResponse is the body of every error response:
//
//	{"error": {"code": "insufficient_funds", "message": "...", "field": "payments[3].amount"}}
//
// code is stable and machine-readable, message is for humans, and field points
// at the offending request field; it is omitted when the error is not about one field.
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

const codeNotImplemented = "not_implemented"

type handler struct {
	svc PaymentService
}

// NewRouter returns the HTTP handler with all API routes registered.
func NewRouter(svc PaymentService) http.Handler {
	h := &handler{svc: svc}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /payments", h.createPayments)
	return mux
}

// createPayments is a stub until request validation and the payment service exist.
func (h *handler) createPayments(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, codeNotImplemented, "bulk payments are not implemented yet", "")
}

func writeError(w http.ResponseWriter, status int, code, message, field string) {
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message, Field: field}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
