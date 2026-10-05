package postgres

import (
	"bytes"
	"database/sql"
	"errors"
	"net"
	"net/url"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"firm-payments/internal/payments"
)

// The worst failure for a payment service: COMMIT reaches PostgreSQL and
// succeeds, but the reply is lost. CreatePayments must report
// ErrOutcomeUnknown and must not retry, because the batch was in fact applied;
// a retry would charge the payer twice.
func TestCreatePaymentsConnectionLostDuringCommit(t *testing.T) {
	db, ctx := openTestDB(t) // direct connection, for setup and checks
	store, proxy := proxiedStore(t)
	retries := 0
	store.onRetry = func(error) { retries++ }

	err := store.CreatePayments(ctx, batch(pinecrestUUID, pay(lopezUUID, 100, "committed, reply lost")))

	if !proxy.dropped.Load() {
		t.Fatal("the proxy never saw COMMIT, so the test did not exercise a lost commit reply")
	}
	if !errors.Is(err, payments.ErrOutcomeUnknown) {
		t.Fatalf("err = %v, want %v", err, payments.ErrOutcomeUnknown)
	}
	if retries != 0 {
		t.Errorf("retries = %d, want 0", retries)
	}

	// The commit did happen, exactly once.
	want := map[string]int64{pinecrestUUID: 5000000 - 100, lopezUUID: 50000 + 100, nairUUID: 200000}
	if got := balances(ctx, t, db); !reflect.DeepEqual(got, want) {
		t.Errorf("balances = %v, want %v (applied exactly once)", got, want)
	}
	if rows := paymentRows(ctx, t, db); len(rows) != 1 {
		t.Errorf("payments rows = %v, want exactly one", rows)
	}
}

// Shows why ErrOutcomeUnknown must never be retried, by the store or by a
// client: running the same batch again after a lost COMMIT reply, as a naive
// retry would, charges the payer twice.
func TestRetryingAfterLostCommitReplyPaysTwice(t *testing.T) {
	db, ctx := openTestDB(t)
	store, proxy := proxiedStore(t)
	b := batch(pinecrestUUID, pay(lopezUUID, 100, "sent twice"))

	err := store.CreatePayments(ctx, b)
	if !proxy.dropped.Load() || !errors.Is(err, payments.ErrOutcomeUnknown) {
		t.Fatalf("first attempt: err = %v, want %v after a lost COMMIT reply", err, payments.ErrOutcomeUnknown)
	}

	// A naive retry: the proxy only drops the first reply, so this one succeeds.
	if err := store.CreatePayments(ctx, b); err != nil {
		t.Fatalf("second attempt: %v", err)
	}

	want := map[string]int64{pinecrestUUID: 5000000 - 2*100, lopezUUID: 50000 + 2*100, nairUUID: 200000}
	if got := balances(ctx, t, db); !reflect.DeepEqual(got, want) {
		t.Errorf("balances = %v, want %v (charged twice)", got, want)
	}
	if rows := paymentRows(ctx, t, db); len(rows) != 2 {
		t.Errorf("payments rows = %v, want two identical rows", rows)
	}
}

// proxiedStore returns a Store whose connections go through a commitDropProxy.
func proxiedStore(t *testing.T) (*Store, *commitDropProxy) {
	t.Helper()
	proxy := startCommitDropProxy(t)
	db, err := sql.Open("postgres", proxy.dsn)
	if err != nil {
		t.Fatalf("open proxied database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db), proxy
}

// commitMessage is how lib/pq sends COMMIT: a simple Query message
// ('Q', int32 length including itself, "COMMIT\x00").
var commitMessage = []byte{'Q', 0, 0, 0, 11, 'C', 'O', 'M', 'M', 'I', 'T', 0}

// commitDropProxy relays TCP traffic between the store and PostgreSQL. The
// first time a client sends COMMIT, the proxy forwards it, waits for
// PostgreSQL's reply, and then closes the connection instead of relaying the
// reply: the transaction is committed, but the client never learns it.
type commitDropProxy struct {
	dsn     string
	target  string
	ln      net.Listener
	dropped atomic.Bool
	wg      sync.WaitGroup
}

func startCommitDropProxy(t *testing.T) *commitDropProxy {
	t.Helper()
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || u.Host == "" {
		t.Skip("TEST_DATABASE_URL must be a postgres:// URL for the proxy test")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &commitDropProxy{target: u.Host, ln: ln}
	u.Host = ln.Addr().String()
	q := u.Query()
	q.Set("sslmode", "disable") // the proxy must see plain protocol messages
	u.RawQuery = q.Encode()
	p.dsn = u.String()

	p.wg.Add(1)
	go p.serve()
	t.Cleanup(func() {
		ln.Close()
		p.wg.Wait()
	})
	return p
}

func (p *commitDropProxy) serve() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return // listener closed
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			client.Close()
			continue
		}
		p.wg.Add(2)
		var dropNextReply atomic.Bool
		closeBoth := func() { client.Close(); server.Close() }

		// client -> server
		go func() {
			defer p.wg.Done()
			defer closeBoth()
			buf := make([]byte, 32<<10)
			for {
				n, err := client.Read(buf)
				if n > 0 {
					if !p.dropped.Load() && bytes.Contains(buf[:n], commitMessage) {
						dropNextReply.Store(true) // before forwarding, so the reply cannot slip through
					}
					if _, werr := server.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()

		// server -> client
		go func() {
			defer p.wg.Done()
			defer closeBoth()
			buf := make([]byte, 32<<10)
			for {
				n, err := server.Read(buf)
				if n > 0 && dropNextReply.Load() {
					p.dropped.Store(true)
					return // PostgreSQL has answered COMMIT; the client never sees it
				}
				if n > 0 {
					if _, werr := client.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
}
