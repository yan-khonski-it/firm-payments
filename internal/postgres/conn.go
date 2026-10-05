package postgres

import (
	"context"
	"database/sql"
	"net"
	"time"

	"github.com/lib/pq"
)

// DefaultIOTimeout bounds every read from and every write to a database
// connection.
//
// A request's context does not fully bound database calls with lib/pq: BEGIN
// runs before lib/pq starts watching the context, and for later statements a
// cancelled context only sends PostgreSQL a separate cancel request. Neither
// helps when the server or the network stops answering, or stops reading a
// large request; the call would wait until the operating system gives up on
// the TCP connection. Bounding each read and write caps that wait.
//
// It sits above the transaction's statement_timeout, so while PostgreSQL is
// responsive its own timeouts fire first. Idle pooled connections are not
// affected, as nothing reads from or writes to them.
const DefaultIOTimeout = 7 * time.Second

// Open returns a connection pool for dsn whose connections never wait longer
// than ioTimeout for a single read or write.
func Open(dsn string, ioTimeout time.Duration) (*sql.DB, error) {
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	connector.Dialer(ioTimeoutDialer{timeout: ioTimeout})
	return sql.OpenDB(connector), nil
}

// ioTimeoutDialer dials ordinary connections and wraps them in ioTimeoutConn.
// lib/pq uses DialContext when it is available.
type ioTimeoutDialer struct {
	timeout time.Duration
}

func (d ioTimeoutDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &ioTimeoutConn{Conn: conn, timeout: d.timeout}, nil
}

func (d ioTimeoutDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d ioTimeoutDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.DialContext(ctx, network, address)
}

// ioTimeoutConn sets a fresh deadline before every read and every write. A
// write needs its own bound: if PostgreSQL stops reading, a large write blocks
// once the send buffers are full, and no read (with its deadline) ever starts.
type ioTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *ioTimeoutConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *ioTimeoutConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}
