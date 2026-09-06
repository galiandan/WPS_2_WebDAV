package httpserver

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
)

// TestSlotListenerClosesThirdConnection pins D-09 end to end: while two
// connections hold the process-wide slots, a third connection is closed at
// accept without ever receiving an HTTP response.
func TestSlotListenerClosesThirdConnection(t *testing.T) {
	b, err := budget.New(budget.Config{MaxConnections: 2, MaxUploads: 1, MaxDownloads: 1, TransferWaitTimeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		w.Write([]byte("done"))
	})
	// Reserve a free port; Listen validates the configured port range.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	listener, server, err := Listen(ServerConfig{
		Bind:           "127.0.0.1",
		Port:           port,
		RequestTimeout: time.Second,
		TransferBudget: b,
		Handler:        handler,
	})
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	defer server.Close()

	dial := func() net.Conn {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	c1 := dial()
	defer c1.Close()
	if _, err := c1.Write([]byte("GET /a HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	c2 := dial()
	defer c2.Close()
	if _, err := c2.Write([]byte("GET /a HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// Wait until both handlers are in flight, so both slots are held.
	time.Sleep(300 * time.Millisecond)

	c3 := dial()
	defer c3.Close()
	if _, err := c3.Write([]byte("GET /b HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	c3.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, err := c3.Read(buf)
	if n > 0 {
		t.Fatalf("the over-limit connection must be closed without a response, got %q", buf[:n])
	}
}
