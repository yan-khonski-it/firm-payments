package postgres

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// newPipeConn returns an ioTimeoutConn over an in-memory pipe, and the peer
// end. On a pipe, a write blocks until the peer reads and a read blocks until
// the peer writes, so a peer that does nothing stands for a database that has
// stopped reading or answering.
func newPipeConn(t *testing.T, timeout time.Duration) (*ioTimeoutConn, net.Conn) {
	t.Helper()
	client, peer := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		peer.Close()
	})
	return &ioTimeoutConn{Conn: client, timeout: timeout}, peer
}

func TestIOTimeoutConnBoundsBlockedOperations(t *testing.T) {
	const timeout = 100 * time.Millisecond
	tests := []struct {
		name string
		op   func(c *ioTimeoutConn) error
	}{
		{"write to a peer that stops reading", func(c *ioTimeoutConn) error {
			_, err := c.Write([]byte("INSERT INTO payments ..."))
			return err
		}},
		{"read from a peer that stops answering", func(c *ioTimeoutConn) error {
			_, err := c.Read(make([]byte, 16))
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, _ := newPipeConn(t, timeout)

			done := make(chan error, 1)
			go func() { done <- tt.op(conn) }()

			select {
			case err := <-done:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("err = %v, want %v", err, os.ErrDeadlineExceeded)
				}
			case <-time.After(time.Second):
				// Unblock the operation so the test ends instead of hanging.
				conn.Close()
				<-done
				t.Fatalf("still blocked after 1s, want %v after about %v", os.ErrDeadlineExceeded, timeout)
			}
		})
	}
}

// The timeout applies to each operation, not to the connection's lifetime:
// traffic that keeps moving is never cut off.
func TestIOTimeoutConnAllowsActiveTraffic(t *testing.T) {
	const timeout = 100 * time.Millisecond
	conn, peer := newPipeConn(t, timeout)
	go io.Copy(io.Discard, peer) // the peer keeps reading

	for i := 0; i < 5; i++ {
		time.Sleep(60 * time.Millisecond) // 300ms in total, three times the timeout
		if _, err := conn.Write([]byte("x")); err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
	}
}
