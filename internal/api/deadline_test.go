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

// sendSlowly writes the request headers, then the body in two halves with a
// pause between them, and returns the response.
func sendSlowly(t *testing.T, addr string, pause time.Duration) *http.Response {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	body := sampleRequest
	fmt.Fprintf(conn, "POST /payments HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(body))
	io.WriteString(conn, body[:len(body)/2])
	time.Sleep(pause)
	io.WriteString(conn, body[len(body)/2:]) // may fail if the server already gave up

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

	resp := sendSlowly(t, srv.Listener.Addr().String(), time.Second)

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
	resp := sendSlowly(t, srv.Listener.Addr().String(), pause)

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
