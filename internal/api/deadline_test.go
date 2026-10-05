package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"firm-payments/internal/payments"
)

// deadlineService records the context deadline of each call. It uses a
// channel because it runs on the server's goroutine.
type deadlineService struct {
	deadlines chan time.Time
}

func (s deadlineService) CreatePayments(ctx context.Context, _ payments.Batch) error {
	deadline, _ := ctx.Deadline()
	s.deadlines <- deadline
	return nil
}

// startSlowRequest sends the request headers and only the first half of the
// body. It returns the connection and the rest of the body, which the caller
// sends later or not at all.
func startSlowRequest(t *testing.T, addr string) (net.Conn, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	body := sampleRequest
	head := fmt.Sprintf("POST /payments HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(body))
	if _, err := io.WriteString(conn, head+body[:len(body)/2]); err != nil {
		t.Fatalf("send headers and the first half of the body: %v", err)
	}
	return conn, body[len(body)/2:]
}

func readResponse(t *testing.T, conn net.Conn) *http.Response {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// A body that arrives slower than the request budget is rejected with 408,
// and the payment service is never called.
func TestCreatePaymentsSlowBodyTimesOut(t *testing.T) {
	svc := deadlineService{deadlines: make(chan time.Time, 1)}
	srv := httptest.NewServer(newRouter(svc, 300*time.Millisecond))
	defer srv.Close()

	// Send half the body and stop. The response is read while the upload is
	// stalled: writing after the server gave up would make it reset the
	// connection, and on Windows a reset discards the unread 408.
	conn, _ := startSlowRequest(t, srv.Listener.Addr().String())
	resp := readResponse(t, conn)

	if resp.StatusCode != http.StatusRequestTimeout {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusRequestTimeout)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"code":"`+codeRequestTimeout+`"`) {
		t.Errorf("body = %s, want code %s", b, codeRequestTimeout)
	}
	select {
	case <-svc.deadlines:
		t.Error("the payment service was called")
	default:
	}
}

// The budget starts before the body is read: time spent receiving a slow body
// is taken from the time left for the payment itself.
func TestCreatePaymentsBudgetIncludesReadingTheBody(t *testing.T) {
	const timeout, pause = 2 * time.Second, 500 * time.Millisecond
	svc := deadlineService{deadlines: make(chan time.Time, 1)}
	srv := httptest.NewServer(newRouter(svc, timeout))
	defer srv.Close()

	start := time.Now()
	conn, rest := startSlowRequest(t, srv.Listener.Addr().String())
	time.Sleep(pause)
	if _, err := io.WriteString(conn, rest); err != nil {
		t.Fatalf("send the rest of the body: %v", err)
	}
	resp := readResponse(t, conn)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	// Starting the budget after reading would put the deadline about pause
	// later; 100ms of slack covers connection setup.
	deadline := <-svc.deadlines
	if limit := start.Add(timeout + 100*time.Millisecond); deadline.After(limit) {
		t.Errorf("service deadline is %v after the request started, want about %v",
			deadline.Sub(start), timeout)
	}
}
