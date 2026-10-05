package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"firm-payments/internal/api"
	"firm-payments/internal/postgres"
	"firm-payments/internal/testdb"
)

// End-to-end tests: real HTTP servers backed by PostgreSQL. Like the store's
// integration tests, they need TEST_DATABASE_URL and wipe that database.

// sampleBody is the bulk payment from payment-body.json: $13,251.25 in total,
// with two payments to the same payee (Nair).
const sampleBody = `{
	"payer_firm_uuid": "3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41",
	"payments": [
		{"amount": "6250", "payee_firm_uuid": "e5f18b3c-2a9d-4c07-8e6b-1d4a7f9c3b25", "description": "Overflow returns, August 2026"},
		{"amount": "5800.5", "payee_firm_uuid": "e5f18b3c-2a9d-4c07-8e6b-1d4a7f9c3b25", "description": "Amended returns, August 2026"},
		{"amount": "1200.75", "payee_firm_uuid": "8b2e4c71-0d3a-4f6e-b1c9-5a7d2e9f4c10", "description": "Bookkeeping cleanup, 3 clients"}
	]
}`

const (
	pinecrest = "3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41"
	lopez     = "8b2e4c71-0d3a-4f6e-b1c9-5a7d2e9f4c10"
	nair      = "e5f18b3c-2a9d-4c07-8e6b-1d4a7f9c3b25"
)

var seedBalances = map[string]int64{pinecrest: 5000000, lopez: 50000, nair: 200000}

// afterSamples returns the balances after n successful sample requests.
func afterSamples(n int64) map[string]int64 {
	return map[string]int64{
		pinecrest: 5000000 - n*1325125,
		lopez:     50000 + n*120075,
		nair:      200000 + n*1205050,
	}
}

func TestEndToEndSampleRequest(t *testing.T) {
	db, ctx := resetTestDB(t)
	srv := newInstance(t)

	status, body := post(t, srv.URL, sampleBody)

	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	want := map[string]any{"payer_firm_uuid": pinecrest, "payment_count": float64(3), "total_amount": "13251.25"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("body = %v, want %v", got, want)
	}
	if got := balances(ctx, t, db); !reflect.DeepEqual(got, afterSamples(1)) {
		t.Errorf("balances = %v, want %v", got, afterSamples(1))
	}
	if n := paymentCount(ctx, t, db); n != 3 {
		t.Errorf("payments rows = %d, want 3 (one per entry, even for the same payee)", n)
	}
}

// Pinecrest's $50,000 covers three $13,251.25 batches; the fourth gets 422 and
// changes nothing.
func TestEndToEndInsufficientFunds(t *testing.T) {
	db, ctx := resetTestDB(t)
	srv := newInstance(t)

	for i, want := range []int{201, 201, 201, 422} {
		if status, body := post(t, srv.URL, sampleBody); status != want {
			t.Fatalf("request %d: status = %d, want %d; body = %s", i+1, status, want, body)
		}
	}
	if got := balances(ctx, t, db); !reflect.DeepEqual(got, afterSamples(3)) {
		t.Errorf("balances = %v, want %v", got, afterSamples(3))
	}
	if n := paymentCount(ctx, t, db); n != 9 {
		t.Errorf("payments rows = %d, want 9", n)
	}
}

func TestEndToEndUnknownPayee(t *testing.T) {
	db, ctx := resetTestDB(t)
	srv := newInstance(t)
	body := strings.Replace(sampleBody, lopez, "00000000-0000-4000-8000-000000000000", 1)

	status, resp := post(t, srv.URL, body)

	if status != http.StatusNotFound || !strings.Contains(resp, `"field":"payments[2].payee_firm_uuid"`) {
		t.Errorf("status = %d, body = %s; want 404 naming payments[2].payee_firm_uuid", status, resp)
	}
	if got := balances(ctx, t, db); !reflect.DeepEqual(got, seedBalances) {
		t.Errorf("balances = %v, want unchanged %v", got, seedBalances)
	}
}

// Two independent service instances (separate connection pools, as behind a
// load balancer) receive ten concurrent copies of the sample. Exactly three
// fit Pinecrest's balance, whichever instance handles them.
func TestEndToEndTwoInstancesConcurrently(t *testing.T) {
	db, ctx := resetTestDB(t)
	instances := []*httptest.Server{newInstance(t), newInstance(t)}

	const requests = 10
	statuses := make([]int, requests)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			statuses[i], _ = post(t, instances[i%len(instances)].URL, sampleBody)
		}(i)
	}
	close(start)
	wg.Wait()

	count := map[int]int{}
	for _, s := range statuses {
		count[s]++
	}
	if count[201] != 3 || count[422] != 7 || len(count) != 2 {
		t.Errorf("status counts = %v, want 3 x 201 and 7 x 422", count)
	}
	if got := balances(ctx, t, db); !reflect.DeepEqual(got, afterSamples(3)) {
		t.Errorf("balances = %v, want %v", got, afterSamples(3))
	}
	var total int64
	for _, b := range balances(ctx, t, db) {
		total += b
	}
	if total != 5250000 {
		t.Errorf("total balance = %d, want 5250000 (money must be conserved)", total)
	}
}

// resetTestDB resets the shared test database to the seed data; see testdb.Open.
func resetTestDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	return testdb.Open(t, 20*time.Second)
}

// newInstance starts one service instance with its own connection pool.
func newInstance(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(api.NewRouter(postgres.NewStore(openDB(t, os.Getenv("TEST_DATABASE_URL")))))
	t.Cleanup(srv.Close)
	return srv
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	db.SetMaxOpenConns(10)
	t.Cleanup(func() { db.Close() })
	return db
}

func post(t *testing.T, baseURL, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(baseURL+"/payments", "application/json", strings.NewReader(body))
	if err != nil {
		t.Errorf("POST /payments: %v", err)
		return 0, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func balances(ctx context.Context, t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT uuid, balance_cents FROM firms`)
	if err != nil {
		t.Fatalf("query balances: %v", err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var uuid string
		var balance int64
		if err := rows.Scan(&uuid, &balance); err != nil {
			t.Fatalf("scan balance: %v", err)
		}
		got[uuid] = balance
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read balances: %v", err)
	}
	return got
}

func paymentCount(ctx context.Context, t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM payments`).Scan(&n); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	return n
}
