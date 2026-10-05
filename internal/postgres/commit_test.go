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

	if !proxy.triggered.Load() {
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
	if !proxy.triggered.Load() || !errors.Is(err, payments.ErrOutcomeUnknown) {
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

// proxiedStore returns a Store whose connections go through a faultProxy that
// drops the reply to the first COMMIT.
func proxiedStore(t *testing.T) (*Store, *faultProxy) {
	t.Helper()
	proxy := startFaultProxy(t, commitMessage, dropReply)
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

// faultMode is what a faultProxy does once a client sends its trigger message.
type faultMode int

const (
	// dropReply forwards the trigger, waits for PostgreSQL's reply and closes
	// the connection instead of relaying it. Only the first trigger is faulted.
	dropReply faultMode = iota
	// stall forwards the trigger and then never relays another byte on that
	// connection, like a server that stopped answering. Every trigger is
	// faulted.
	stall
)

// faultProxy relays TCP traffic between the store and PostgreSQL and injects
// one kind of fault, triggered by a message the client sends.
type faultProxy struct {
	dsn       string
	target    string
	ln        net.Listener
	trigger   []byte
	mode      faultMode
	triggered atomic.Bool // the fault has happened at least once
	wg        sync.WaitGroup
}

func startFaultProxy(t *testing.T, trigger []byte, mode faultMode) *faultProxy {
	t.Helper()
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || u.Host == "" {
		t.Skip("TEST_DATABASE_URL must be a postgres:// URL for the proxy tests")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &faultProxy{target: u.Host, ln: ln, trigger: trigger, mode: mode}
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

func (p *faultProxy) serve() {
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
		var faulted atomic.Bool // this connection has sent the trigger
		closeBoth := func() { client.Close(); server.Close() }

		// client -> server
		go func() {
			defer p.wg.Done()
			defer closeBoth()
			buf := make([]byte, 32<<10)
			for {
				n, err := client.Read(buf)
				if n > 0 {
					armed := p.mode == stall || !p.triggered.Load()
					if armed && bytes.Contains(buf[:n], p.trigger) {
						faulted.Store(true) // before forwarding, so the reply cannot slip through
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
				if n > 0 && faulted.Load() {
					p.triggered.Store(true)
					if p.mode == dropReply {
						return // PostgreSQL has answered; the client never sees it
					}
					continue // stall: swallow everything from now on
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
