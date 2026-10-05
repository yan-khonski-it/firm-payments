package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"firm-payments/internal/payments"
)

const (
	maxPayments = 1000
	// maxDescriptionLength is in characters (runes), matching char_length() in
	// the payments table constraint.
	maxDescriptionLength = 500
	// maxAmountCents is the largest value the INTEGER amount and balance columns hold.
	maxAmountCents = math.MaxInt32
	// maxBodyBytes allows maxPayments entries with fully Unicode-escaped descriptions.
	maxBodyBytes = 4_194_304 // 4 MiB
)

const (
	codeUnsupportedMediaType = "unsupported_media_type"
	codeRequestTooLarge      = "request_too_large"
	codeRequestTimeout       = "request_timeout"
	codeInvalidJSON          = "invalid_json"
	codeUnknownField         = "unknown_field"
	codeMissingField         = "missing_field"
	codeInvalidType          = "invalid_type"
	codeInvalidUUID          = "invalid_uuid"
	codeInvalidAmount        = "invalid_amount"
	codeAmountTooLarge       = "amount_too_large"
	codeDescriptionTooLong   = "description_too_long"
	codeInvalidCharacter     = "invalid_character"
	codeNoPayments           = "no_payments"
	codeTooManyPayments      = "too_many_payments"
	codeSelfPayment          = "self_payment"
)

// requestError is a client error found while reading or validating a request.
type requestError struct {
	status  int
	code    string
	message string
	field   string
}

func badRequest(code, field, format string, args ...any) *requestError {
	return &requestError{
		status:  http.StatusBadRequest,
		code:    code,
		field:   field,
		message: fmt.Sprintf(format, args...),
	}
}

// readBatch checks the Content-Type, reads the body with a size limit and
// validates it. It does not touch the database: whether the firms exist and
// whether the payer can cover the batch are checked inside the transaction.
func readBatch(w http.ResponseWriter, r *http.Request) (payments.Batch, *requestError) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return payments.Batch{}, &requestError{
			status:  http.StatusUnsupportedMediaType,
			code:    codeUnsupportedMediaType,
			message: "Content-Type must be application/json",
		}
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return payments.Batch{}, &requestError{
				status:  http.StatusRequestEntityTooLarge,
				code:    codeRequestTooLarge,
				message: fmt.Sprintf("request body must not exceed %d bytes", maxBodyBytes),
			}
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return payments.Batch{}, &requestError{
				status:  http.StatusRequestTimeout,
				code:    codeRequestTimeout,
				message: "the request body did not arrive in time; nothing was paid",
			}
		}
		return payments.Batch{}, badRequest(codeInvalidJSON, "", "could not read request body")
	}

	return parseBatch(body)
}

// parseBatch validates a request body and converts it into a payments.Batch.
// Validation stops at the first error, checked in document order.
func parseBatch(body []byte) (payments.Batch, *requestError) {
	obj, rerr := parseBody(body)
	if rerr != nil {
		return payments.Batch{}, rerr
	}
	if rerr := checkKeys(obj, "", "payer_firm_uuid", "payments"); rerr != nil {
		return payments.Batch{}, rerr
	}

	payer, rerr := uuidField(obj, "payer_firm_uuid", "payer_firm_uuid")
	if rerr != nil {
		return payments.Batch{}, rerr
	}

	items, rerr := arrayField(obj, "payments", "payments")
	if rerr != nil {
		return payments.Batch{}, rerr
	}
	if len(items) == 0 {
		return payments.Batch{}, badRequest(codeNoPayments, "payments", "payments must contain at least one payment")
	}
	if len(items) > maxPayments {
		return payments.Batch{}, badRequest(codeTooManyPayments, "payments",
			"payments must contain at most %d payments, got %d", maxPayments, len(items))
	}

	batch := payments.Batch{
		PayerFirmUUID: payer,
		Payments:      make([]payments.Payment, 0, len(items)),
	}
	for i, item := range items {
		p, rerr := parsePayment(item, fmt.Sprintf("payments[%d]", i), payer)
		if rerr != nil {
			return payments.Batch{}, rerr
		}
		batch.Payments = append(batch.Payments, p)
	}
	return batch, nil
}

func parsePayment(raw json.RawMessage, field, payer string) (payments.Payment, *requestError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return payments.Payment{}, badRequest(codeInvalidType, field, "%s must be an object", field)
	}
	if rerr := checkKeys(obj, field, "amount", "payee_firm_uuid", "description"); rerr != nil {
		return payments.Payment{}, rerr
	}

	amountField := field + ".amount"
	amount, rerr := stringField(obj, "amount", amountField)
	if rerr != nil {
		return payments.Payment{}, rerr
	}
	cents, err := parseAmountCents(amount)
	switch {
	case errors.Is(err, errAmountTooLarge):
		return payments.Payment{}, badRequest(codeAmountTooLarge, amountField, "%s %v", amountField, err)
	case err != nil:
		return payments.Payment{}, badRequest(codeInvalidAmount, amountField, "%s %v", amountField, err)
	}

	payeeField := field + ".payee_firm_uuid"
	payee, rerr := uuidField(obj, "payee_firm_uuid", payeeField)
	if rerr != nil {
		return payments.Payment{}, rerr
	}
	if payee == payer {
		return payments.Payment{}, badRequest(codeSelfPayment, payeeField, "%s must differ from payer_firm_uuid", payeeField)
	}

	descriptionField := field + ".description"
	description, rerr := stringField(obj, "description", descriptionField)
	if rerr != nil {
		return payments.Payment{}, rerr
	}
	// PostgreSQL TEXT cannot store NUL characters; reject them rather than
	// silently changing the description.
	if strings.ContainsRune(description, 0) {
		return payments.Payment{}, badRequest(codeInvalidCharacter, descriptionField,
			`%s must not contain NUL (\u0000) characters`, descriptionField)
	}
	if n := utf8.RuneCountInString(description); n > maxDescriptionLength {
		return payments.Payment{}, badRequest(codeDescriptionTooLong, descriptionField,
			"%s must be at most %d characters, got %d", descriptionField, maxDescriptionLength, n)
	}

	return payments.Payment{PayeeFirmUUID: payee, AmountCents: cents, Description: description}, nil
}

// parseBody decodes the request body as a single JSON object. Keys are kept
// exactly as sent, unlike struct decoding, which matches field names
// case-insensitively. For a duplicated key the last value wins.
func parseBody(body []byte) (map[string]json.RawMessage, *requestError) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, badRequest(codeInvalidJSON, "", "request body is empty")
	}
	// JSON must be UTF-8. Without this check, json.Unmarshal would silently
	// replace invalid bytes with U+FFFD and store a changed description.
	if !utf8.Valid(body) {
		return nil, badRequest(codeInvalidJSON, "", "request body is not valid UTF-8")
	}

	var obj map[string]json.RawMessage
	err := json.Unmarshal(body, &obj)
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return nil, badRequest(codeInvalidJSON, "", "request body is not valid JSON: %v", err)
	}
	if err != nil || obj == nil {
		return nil, badRequest(codeInvalidJSON, "", "request body must be a JSON object")
	}
	return obj, nil
}

// checkKeys rejects keys outside allowed, reporting the first one in sorted
// order so the error is deterministic.
func checkKeys(obj map[string]json.RawMessage, prefix string, allowed ...string) *requestError {
	var unknown []string
	for key := range obj {
		known := false
		for _, a := range allowed {
			if key == a {
				known = true
				break
			}
		}
		if !known {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	field := unknown[0]
	if prefix != "" {
		field = prefix + "." + field
	}
	return badRequest(codeUnknownField, field, "unknown field %s", field)
}

// stringField returns obj[key] as a string. A missing key and null are both
// reported as missing; any other non-string value is a type error.
func stringField(obj map[string]json.RawMessage, key, field string) (string, *requestError) {
	raw, ok := obj[key]
	if !ok || isNull(raw) {
		return "", badRequest(codeMissingField, field, "%s is required", field)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", badRequest(codeInvalidType, field, "%s must be a string", field)
	}
	return s, nil
}

func uuidField(obj map[string]json.RawMessage, key, field string) (string, *requestError) {
	s, rerr := stringField(obj, key, field)
	if rerr != nil {
		return "", rerr
	}
	id, ok := parseUUID(s)
	if !ok {
		return "", badRequest(codeInvalidUUID, field,
			"%s must be a UUID in the form xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx", field)
	}
	return id, nil
}

func arrayField(obj map[string]json.RawMessage, key, field string) ([]json.RawMessage, *requestError) {
	raw, ok := obj[key]
	if !ok || isNull(raw) {
		return nil, badRequest(codeMissingField, field, "%s is required", field)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, badRequest(codeInvalidType, field, "%s must be an array", field)
	}
	return items, nil
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// parseUUID accepts the hyphenated 8-4-4-4-12 hex form in either case and
// returns it lowercased, the form stored in firms.uuid.
func parseUUID(s string) (string, bool) {
	if len(s) != 36 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return "", false
			}
		}
	}
	return strings.ToLower(s), true
}

var amountPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,2})?$`)

var (
	errAmountFormat   = errors.New(`must be a decimal number of dollars with at most 2 decimal places, such as "1200.75"`)
	errAmountZero     = errors.New("must be greater than 0")
	errAmountTooLarge = errors.New("must not exceed 21474836.47")
)

// parseAmountCents converts a USD amount such as "1200.75" into cents using
// integer arithmetic only. Leading zeros are accepted ("007.50" is 750).
func parseAmountCents(s string) (int64, error) {
	if !amountPattern.MatchString(s) {
		return 0, errAmountFormat
	}

	whole, frac, _ := strings.Cut(s, ".")
	whole = strings.TrimLeft(whole, "0")
	// The largest allowed amount has 8 whole digits; checking the length first
	// keeps ParseInt from overflowing on very long inputs.
	if len(whole) > 8 {
		return 0, errAmountTooLarge
	}

	var dollars int64
	if whole != "" {
		var err error
		if dollars, err = strconv.ParseInt(whole, 10, 64); err != nil {
			return 0, errAmountFormat
		}
	}
	frac += strings.Repeat("0", 2-len(frac))
	fracCents, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, errAmountFormat
	}

	cents := dollars*100 + fracCents
	switch {
	case cents == 0:
		return 0, errAmountZero
	case cents > maxAmountCents:
		return 0, errAmountTooLarge
	}
	return cents, nil
}
