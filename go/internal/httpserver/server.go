package httpserver

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
)

// ServerConfig mirrors Python's create_server parameters. The connection
// slots come from the process-wide ResourceBudget (D-03), whose
// MaxConnections value plays the role of create_server's max_connections.
type ServerConfig struct {
	Bind           string
	Port           int
	RequestTimeout time.Duration
	TransferBudget *budget.Budget
	Handler        http.Handler
}

// Listen validates the configuration, opens the listening socket behind the
// connection-slot gate, and returns the listener and the configured server
// without serving. Mirrors create_server: no threads start and no network
// calls happen beyond the bind.
func Listen(config ServerConfig) (net.Listener, *http.Server, error) {
	if config.Port < 1 || config.Port > 65535 {
		return nil, nil, fmt.Errorf("port must be between 1 and 65535")
	}
	if config.RequestTimeout <= 0 {
		return nil, nil, fmt.Errorf("request_timeout must be positive")
	}
	if config.TransferBudget == nil {
		return nil, nil, fmt.Errorf("a transfer budget is required")
	}
	if config.Handler == nil {
		return nil, nil, fmt.Errorf("a handler is required")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(config.Bind, strconv.Itoa(config.Port)))
	if err != nil {
		return nil, nil, err
	}
	// Python applies its socket timeout to every operation on the
	// connection. Go cannot bound a streaming body or response write the
	// same way without killing legitimate long transfers, so the server
	// bounds the header phase and the idle gap between requests; body and
	// response deadlines are the handlers' business.
	server := &http.Server{
		Handler:           config.Handler,
		ReadHeaderTimeout: config.RequestTimeout,
		IdleTimeout:       config.RequestTimeout,
	}
	return newSlotListener(listener, config.TransferBudget), server, nil
}

// slotListener gates connections on the process-wide budget: over-limit
// connections are closed at accept without holding a slot (D-09), and every
// accepted connection releases its slot exactly once when it closes.
type slotListener struct {
	net.Listener
	budget *budget.Budget
}

func newSlotListener(listener net.Listener, transferBudget *budget.Budget) *slotListener {
	return &slotListener{Listener: listener, budget: transferBudget}
}

func (l *slotListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		release, ok := l.budget.TryAcquireConnection()
		if !ok {
			_ = conn.Close()
			continue
		}
		return &slotConn{Conn: conn, release: release}, nil
	}
}

type slotConn struct {
	net.Conn
	release     func()
	releaseOnce sync.Once
}

func (c *slotConn) Close() error {
	c.releaseOnce.Do(func() { c.release() })
	return c.Conn.Close()
}
