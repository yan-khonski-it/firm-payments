package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"firm-payments/internal/payments"
)

const (
	payerUUID  = "3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41"
	payeeUUID  = "8b2e4c71-0d3a-4f6e-b1c9-5a7d2e9f4c10"
	payee2UUID = "e5f18b3c-2a9d-4c07-8e6b-1d4a7f9c3b25"
)

// q encodes s as a JSON string literal.
func q(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// payment builds one payment object from raw JSON values.
func payment(amount, payee, description string) string {
	return `{"amount":` + amount + `,"payee_firm_uuid":` + payee + `,"description":` + description + `}`
}

// request builds a request body from a raw JSON payer value and raw payment objects.
func request(payer string, items ...string) string {
	return `{"payer_firm_uuid":` + payer + `,"payments":[` + strings.Join(items, ",") + `]}`
}

var validPayment = payment(q("10.00"), q(payeeUUID), q("Invoice 1"))

func TestParseAmountCents(t *testing.T) {
	valid := map[string]int64{
		"300":                    30000,
		"5800.5":                 580050,
		"1200.75":                120075,
		"0.01":                   1,
		"0.1":                    10,
		"007.50":                 750,
		"21474836.47":            2147483647,
		"0000000000021474836.47": 2147483647,
	}
	for in, want := range valid {
		got, err := parseAmountCents(in)
		if err != nil || got != want {
			t.Errorf("parseAmountCents(%q) = %d, %v; want %d, nil", in, got, err, want)
		}
	}

	invalid := map[string]error{
		"":                     errAmountFormat,
		"-1":                   errAmountFormat,
		"+1":                   errAmountFormat,
		"1.":                   errAmountFormat,
		".5":                   errAmountFormat,
		"1.234":                errAmountFormat,
		"1e3":                  errAmountFormat,
		"NaN":                  errAmountFormat,
		"Infinity":             errAmountFormat,
		" 1":                   errAmountFormat,
		"1 ":                   errAmountFormat,
		"1\n":                  errAmountFormat,
		"1,000":                errAmountFormat,
		"0x10":                 errAmountFormat,
		"１":                    errAmountFormat,
		"0":                    errAmountZero,
		"0.00":                 errAmountZero,
		"000":                  errAmountZero,
		"21474836.48":          errAmountTooLarge,
		"100000000":            errAmountTooLarge,
		"99999999999999999999": errAmountTooLarge,
	}
	for in, want := range invalid {
		got, err := parseAmountCents(in)
		if !errors.Is(err, want) {
			t.Errorf("parseAmountCents(%q) = %d, %v; want error %v", in, got, err, want)
		}
	}
}

func TestParseUUID(t *testing.T) {
	valid := map[string]string{
		payerUUID:                              payerUUID,
		"3F1C9A2E-7B4D-4C1E-9A55-2D8E6F0B7C41": payerUUID,
	}
	for in, want := range valid {
		if got, ok := parseUUID(in); !ok || got != want {
			t.Errorf("parseUUID(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}

	invalid := []string{
		"",
		"3f1c9a2e7b4d4c1e9a552d8e6f0b7c41",
		"{3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41}",
		"urn:uuid:3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41",
		"3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c4",
		"3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c411",
		"3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c4g",
		"3f1c9a2e+7b4d-4c1e-9a55-2d8e6f0b7c41",
		" 3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c4",
	}
	for _, in := range invalid {
		if got, ok := parseUUID(in); ok {
			t.Errorf("parseUUID(%q) = %q, true; want invalid", in, got)
		}
	}
}

func TestParseBatchValid(t *testing.T) {
	body := request(q(strings.ToUpper(payerUUID)),
		payment(q("1200.75"), q(payeeUUID), q("Bookkeeping cleanup, 3 clients")),
		payment(q("300"), q(payee2UUID), q("Referral fee, 2 clients")),
		payment(q("5800.5"), q(strings.ToUpper(payee2UUID)), q("")),
	)

	got, rerr := parseBatch([]byte(body))
	if rerr != nil {
		t.Fatalf("parseBatch: unexpected error %+v", *rerr)
	}

	want := payments.Batch{
		PayerFirmUUID: payerUUID,
		Payments: []payments.Payment{
			{PayeeFirmUUID: payeeUUID, AmountCents: 120075, Description: "Bookkeeping cleanup, 3 clients"},
			{PayeeFirmUUID: payee2UUID, AmountCents: 30000, Description: "Referral fee, 2 clients"},
			{PayeeFirmUUID: payee2UUID, AmountCents: 580050, Description: ""},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseBatch =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseBatchLimits(t *testing.T) {
	maxItems := make([]string, maxPayments)
	for i := range maxItems {
		maxItems[i] = validPayment
	}
	maxDescription := payment(q("1"), q(payeeUUID), q(strings.Repeat("é", maxDescriptionLength)))

	for name, body := range map[string]string{
		"1000 payments":             request(q(payerUUID), maxItems...),
		"500-character description": request(q(payerUUID), maxDescription),
	} {
		if _, rerr := parseBatch([]byte(body)); rerr != nil {
			t.Errorf("%s: unexpected error %+v", name, *rerr)
		}
	}
}

func TestParseBatchInvalid(t *testing.T) {
	tooMany := make([]string, maxPayments+1)
	for i := range tooMany {
		tooMany[i] = validPayment
	}

	tests := []struct {
		name      string
		body      string
		wantCode  string
		wantField string
	}{
		{"empty body", "", codeInvalidJSON, ""},
		{"whitespace body", " \n ", codeInvalidJSON, ""},
		{"malformed JSON", `{"payments":`, codeInvalidJSON, ""},
		{"trailing data", request(q(payerUUID), validPayment) + ` {}`, codeInvalidJSON, ""},
		{"array body", `[]`, codeInvalidJSON, ""},
		{"null body", `null`, codeInvalidJSON, ""},
		{"string body", `"x"`, codeInvalidJSON, ""},
		{"invalid UTF-8 in a value", request(q(payerUUID), payment(q("1"), q(payeeUUID), "\"Invoice \xff\"")), codeInvalidJSON, ""},
		{"invalid UTF-8 in a key", "{\"payer\xc3\":1}", codeInvalidJSON, ""},

		{"unknown top-level field", `{"payer_firm_uuid":` + q(payerUUID) + `,"payments":[` + validPayment + `],"extra":1}`, codeUnknownField, "extra"},
		{"field name in wrong case", `{"PAYER_FIRM_UUID":` + q(payerUUID) + `,"payments":[` + validPayment + `]}`, codeUnknownField, "PAYER_FIRM_UUID"},

		{"payer missing", `{"payments":[` + validPayment + `]}`, codeMissingField, "payer_firm_uuid"},
		{"payer null", request(`null`, validPayment), codeMissingField, "payer_firm_uuid"},
		{"payer number", request(`42`, validPayment), codeInvalidType, "payer_firm_uuid"},
		{"payer empty", request(q(""), validPayment), codeInvalidUUID, "payer_firm_uuid"},
		{"payer not a UUID", request(q("pinecrest"), validPayment), codeInvalidUUID, "payer_firm_uuid"},

		{"payments missing", `{"payer_firm_uuid":` + q(payerUUID) + `}`, codeMissingField, "payments"},
		{"payments null", `{"payer_firm_uuid":` + q(payerUUID) + `,"payments":null}`, codeMissingField, "payments"},
		{"payments object", `{"payer_firm_uuid":` + q(payerUUID) + `,"payments":{}}`, codeInvalidType, "payments"},
		{"payments empty", request(q(payerUUID)), codeNoPayments, "payments"},
		{"too many payments", request(q(payerUUID), tooMany...), codeTooManyPayments, "payments"},
		{"payment not an object", request(q(payerUUID), `1`), codeInvalidType, "payments[0]"},
		{"payment null", request(q(payerUUID), `null`), codeInvalidType, "payments[0]"},
		{"unknown payment field", request(q(payerUUID), `{"amout":"1","payee_firm_uuid":`+q(payeeUUID)+`,"description":""}`), codeUnknownField, "payments[0].amout"},

		{"amount missing", request(q(payerUUID), `{"payee_firm_uuid":`+q(payeeUUID)+`,"description":""}`), codeMissingField, "payments[0].amount"},
		{"amount null", request(q(payerUUID), payment(`null`, q(payeeUUID), q(""))), codeMissingField, "payments[0].amount"},
		{"amount number", request(q(payerUUID), payment(`300`, q(payeeUUID), q(""))), codeInvalidType, "payments[0].amount"},
		{"amount zero", request(q(payerUUID), payment(q("0.00"), q(payeeUUID), q(""))), codeInvalidAmount, "payments[0].amount"},
		{"amount negative", request(q(payerUUID), payment(q("-5"), q(payeeUUID), q(""))), codeInvalidAmount, "payments[0].amount"},
		{"amount three decimals", request(q(payerUUID), payment(q("1.234"), q(payeeUUID), q(""))), codeInvalidAmount, "payments[0].amount"},
		{"amount too large", request(q(payerUUID), payment(q("21474836.48"), q(payeeUUID), q(""))), codeAmountTooLarge, "payments[0].amount"},

		{"payee missing", request(q(payerUUID), `{"amount":"1","description":""}`), codeMissingField, "payments[0].payee_firm_uuid"},
		{"payee not a UUID", request(q(payerUUID), payment(q("1"), q("lopez"), q(""))), codeInvalidUUID, "payments[0].payee_firm_uuid"},
		{"payee is payer", request(q(payerUUID), payment(q("1"), q(payerUUID), q(""))), codeSelfPayment, "payments[0].payee_firm_uuid"},
		{"payee is payer, other case", request(q(payerUUID), payment(q("1"), q(strings.ToUpper(payerUUID)), q(""))), codeSelfPayment, "payments[0].payee_firm_uuid"},

		{"description missing", request(q(payerUUID), `{"amount":"1","payee_firm_uuid":`+q(payeeUUID)+`}`), codeMissingField, "payments[0].description"},
		{"description null", request(q(payerUUID), payment(q("1"), q(payeeUUID), `null`)), codeMissingField, "payments[0].description"},
		{"description number", request(q(payerUUID), payment(q("1"), q(payeeUUID), `5`)), codeInvalidType, "payments[0].description"},
		{"description too long", request(q(payerUUID), payment(q("1"), q(payeeUUID), q(strings.Repeat("é", maxDescriptionLength+1)))), codeDescriptionTooLong, "payments[0].description"},
		{"description with NUL", request(q(payerUUID), payment(q("1"), q(payeeUUID), `"Invoice\u00001"`)), codeInvalidCharacter, "payments[0].description"},
		{"description with only NUL", request(q(payerUUID), payment(q("1"), q(payeeUUID), `"\u0000"`)), codeInvalidCharacter, "payments[0].description"},
		{"description over limit with NULs", request(q(payerUUID), payment(q("1"), q(payeeUUID), q(strings.Repeat("x", maxDescriptionLength)+strings.Repeat("\x00", 100)))), codeInvalidCharacter, "payments[0].description"},

		{"error in a later payment", request(q(payerUUID), validPayment, validPayment, payment(q("abc"), q(payeeUUID), q(""))), codeInvalidAmount, "payments[2].amount"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, rerr := parseBatch([]byte(tt.body))
			if rerr == nil {
				t.Fatalf("parseBatch: got no error, want %s", tt.wantCode)
			}
			if rerr.status != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rerr.status, http.StatusBadRequest)
			}
			if rerr.code != tt.wantCode {
				t.Errorf("code = %q, want %q (message: %s)", rerr.code, tt.wantCode, rerr.message)
			}
			if rerr.field != tt.wantField {
				t.Errorf("field = %q, want %q", rerr.field, tt.wantField)
			}
			if rerr.message == "" {
				t.Error("message is empty")
			}
		})
	}
}

func TestCreatePaymentsRequestHandling(t *testing.T) {
	jsonRequest := func(contentType, body string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		return req
	}
	valid := request(q(payerUUID), validPayment)
	tooLarge := `{"payer_firm_uuid":"` + strings.Repeat("a", maxBodyBytes) + `"}`

	tests := []struct {
		name       string
		req        *http.Request
		wantStatus int
		wantCode   string
		wantField  string
	}{
		{"no Content-Type", jsonRequest("", valid), http.StatusUnsupportedMediaType, codeUnsupportedMediaType, ""},
		{"wrong Content-Type", jsonRequest("text/plain", valid), http.StatusUnsupportedMediaType, codeUnsupportedMediaType, ""},
		{"body too large", jsonRequest("application/json", tooLarge), http.StatusRequestEntityTooLarge, codeRequestTooLarge, ""},
		{"invalid body", jsonRequest("application/json", `{`), http.StatusBadRequest, codeInvalidJSON, ""},
		{"invalid field", jsonRequest("application/json", request(q(payerUUID))), http.StatusBadRequest, codeNoPayments, "payments"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(tt.req)

			assertErrorResponse(t, rec, tt.wantStatus, tt.wantCode, tt.wantField)
		})
	}
}
