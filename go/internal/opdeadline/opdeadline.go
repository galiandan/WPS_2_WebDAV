// Package opdeadline ports Python's socket.settimeout to Go connections:
// every read and write re-arms a fresh deadline before the operation, so a
// stalled peer fails the pending operation while a transfer that keeps
// moving is never cut off. The adapter applies it to accepted client
// connections (request_timeout) and to the upstream object-storage
// transports (the WPS timeout), which is what bounds body-phase stalls that
// http.Server and http.Transport timeouts leave unbounded.
package opdeadline

import (
	"net"
	"time"
)

// Conn wraps a net.Conn with the per-operation deadline. The deadline
// setters are safe for concurrent use like the underlying net.Conn, so a
// wrapped connection stays usable from the transport goroutines that share
// one pooled connection.
type Conn struct {
	net.Conn
	timeout time.Duration
}

// Wrap bounds every future read and write on conn to one timeout each.
// Deadline expiry surfaces from the affected operation as
// os.ErrDeadlineExceeded; the next operation re-arms from scratch.
func Wrap(conn net.Conn, timeout time.Duration) *Conn {
	return &Conn{Conn: conn, timeout: timeout}
}

func (c *Conn) Read(p []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *Conn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}
