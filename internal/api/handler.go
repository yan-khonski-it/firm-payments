package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

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
	codeNotFound             = "not_found"
	codeMethodNotAllowed     = "method_not_allowed"
	codePayerNotFound        = "payer_not_found"
	codePayeeNotFound        = "payee_not_found"
	codeInsufficientFunds    = "insufficient_funds"
	codeBalanceLimitExceeded = "balance_limit_exceeded"
	codeBusy                 = "busy"
	codeTimeout              = "timeout"
	codeOutcomeUnknown       = "outcome_unknown"
	codeInternal             = "internal_error"
)

// requestTimeout bounds one payment request from the moment the handler starts:
// reading the body, validation and the database work, retries included. The
// database timeouts sit below it: lock 2s < statement 5s < request 8s.
const requestTimeout = 8 * time.Second

// responseWriteMargin is how long the response may still take to write after
// the request deadline. A COMMIT that started just before the deadline still
// completes, and the client should still hear about it.
const responseWriteMargin = 2 * time.Second

// createPaymentsResponse is the 201 body.
type createPaymentsResponse struct {
	PayerFirmUUID string `json:"payer_firm_uuid"`
	PaymentCount  int    `json:"payment_count"`
	TotalAmount   string `json:"total_amount"`
}

type handler struct {
	svc     PaymentService
	timeout time.Duration
}

// NewRouter returns the HTTP handler with all API routes registered.
func NewRouter(svc PaymentService) http.Handler {
	return newRouter(svc, requestTimeout)
}

// newRouter is NewRouter with a configurable request timeout, for tests.
func newRouter(svc PaymentService, timeout time.Duration) http.Handler {
	h := &handler{svc: svc, timeout: timeout}

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

// createPayments validates the request and executes the batch.
func (h *handler) createPayments(w http.ResponseWriter, r *http.Request) {
	// The budget starts before the body is read, so a slow upload cannot push
	// the database work past the point where the response can still be written.
	deadline := time.Now().Add(h.timeout)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	setConnDeadlines(w, deadline)

	batch, rerr := readBatch(w, r)
	if rerr != nil {
		writeError(w, rerr.status, rerr.code, rerr.message, rerr.field)
		return
	}

	if err := h.svc.CreatePayments(ctx, batch); err != nil {
		writeServiceError(w, batch, err)
		return
	}

	total, _ := batch.TotalCents() // cannot fail: CreatePayments already computed it
	err := writeJSON(w, http.StatusCreated, createPaymentsResponse{
		PayerFirmUUID: batch.PayerFirmUUID,
		PaymentCount:  len(batch.Payments),
		TotalAmount:   formatCents(total),
	})
	if err != nil {
		// The payments are committed; only the confirmation was lost. Nothing is
		// retried here, as that would pay twice.
		log.Printf("payments committed for payer %s (%d payments, %s) but the 201 response was not delivered: %v",
			batch.PayerFirmUUID, len(batch.Payments), formatCents(total), err)
	}
}

// setConnDeadlines makes reading the body stop at the request deadline and
// leaves the response responseWriteMargin after it, replacing the server-wide
// read and write timeouts for this request. Test recorders do not support
// connection deadlines (http.ErrNotSupported); real connections do.
func setConnDeadlines(w http.ResponseWriter, deadline time.Time) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("set read deadline: %v", err)
	}
	if err := rc.SetWriteDeadline(deadline.Add(responseWriteMargin)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("set write deadline: %v", err)
	}
}

// writeServiceError maps a CreatePayments error to a response. Every error
// except ErrOutcomeUnknown means the transaction was rolled back, so the
// messages can say that nothing was paid.
func writeServiceError(w http.ResponseWriter, batch payments.Batch, err error) {
	var notFound *payments.FirmNotFoundError
	switch {
	case errors.As(err, &notFound) && notFound.IsPayer:
		writeError(w, http.StatusNotFound, codePayerNotFound,
			"the payer firm does not exist; nothing was paid", "payer_firm_uuid")
	case errors.As(err, &notFound):
		writeError(w, http.StatusNotFound, codePayeeNotFound,
			"a payee firm does not exist; nothing was paid", payeeField(batch, notFound.FirmUUID))
	case errors.Is(err, payments.ErrInsufficientFunds):
		writeError(w, http.StatusUnprocessableEntity, codeInsufficientFunds,
			"the payer's balance does not cover the total of all payments; nothing was paid", "")
	case errors.Is(err, payments.ErrBalanceLimitExceeded):
		writeError(w, http.StatusUnprocessableEntity, codeBalanceLimitExceeded,
			"a payee's balance would exceed the largest supported balance; nothing was paid", "")
	case errors.Is(err, payments.ErrOutcomeUnknown):
		log.Printf("create payments for payer %s: %v", batch.PayerFirmUUID, err)
		writeError(w, http.StatusInternalServerError, codeOutcomeUnknown,
			"the database connection was lost while committing, so the payments may or may not "+
				"have been applied; check the balances before retrying, or a retry may pay twice", "")
	case errors.Is(err, payments.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, codeBusy,
			"the database is busy; nothing was paid, try again later", "")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, codeTimeout,
			"the request took too long; nothing was paid, try again later", "")
	default:
		log.Printf("create payments for payer %s: %v", batch.PayerFirmUUID, err)
		writeError(w, http.StatusInternalServerError, codeInternal,
			"internal error; nothing was paid", "")
	}
}

// payeeField returns the field path of the first payment to the given payee.
func payeeField(batch payments.Batch, payeeUUID string) string {
	for i, p := range batch.Payments {
		if p.PayeeFirmUUID == payeeUUID {
			return fmt.Sprintf("payments[%d].payee_firm_uuid", i)
		}
	}
	return "payments"
}

// formatCents renders a non-negative amount in cents as dollars, e.g. "13251.25".
func formatCents(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func writeError(w http.ResponseWriter, status int, code, message, field string) {
	err := writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message, Field: field}})
	if err != nil {
		log.Printf("could not deliver the %d %s response: %v", status, code, err)
	}
}

// writeJSON writes v as the response and flushes it. Without the flush the
// small response would sit in the server's buffer until the handler returns,
// and a failed delivery (client gone, write deadline passed) would never be
// reported here.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return err
	}
	if err := http.NewResponseController(w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}
