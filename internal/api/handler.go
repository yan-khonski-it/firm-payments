package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"firm-payments/internal/payments"
)

// PaymentService atomically creates a validated payment batch.
type PaymentService interface {
	CreatePayments(ctx context.Context, batch payments.Batch) error
}

// errorResponse is the JSON envelope returned for API errors:
//
//	{"error":{"code":"insufficient_funds","message":"...","field":"payments[3].amount"}}
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

const (
	codeNotImplemented   = "not_implemented"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
)

type handler struct {
	svc PaymentService
}

// NewRouter returns the HTTP handler with all API routes registered.
func NewRouter(svc PaymentService) http.Handler {
	h := &handler{svc: svc}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /payments", h.createPayments)

	// POST /payments is the only valid route, so all other methods are rejected with 405.
	mux.HandleFunc("/payments", methodNotAllowed(http.MethodPost))
	mux.HandleFunc("/", notFound)
	return mux
}

func methodNotAllowed(allowed ...string) http.HandlerFunc {
	allow := strings.Join(allowed, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, r.Method+" is not allowed; use "+allow, "")
	}
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, codeNotFound, "no such endpoint", "")
}

// createPayments validates the request. Executing the batch is not implemented yet.
func (h *handler) createPayments(w http.ResponseWriter, r *http.Request) {
	if _, rerr := readBatch(w, r); rerr != nil {
		writeError(w, rerr.status, rerr.code, rerr.message, rerr.field)
		return
	}
	writeError(w, http.StatusNotImplemented, codeNotImplemented, "the request is valid, but executing payments is not implemented yet", "")
}

func writeError(w http.ResponseWriter, status int, code, message, field string) {
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message, Field: field}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
