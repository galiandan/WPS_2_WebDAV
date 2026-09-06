// The body-phase deadline tests pin the socket-settimeout parity on the
// server side: a stalled request body is cut at the request timeout
// instead of holding its connection forever, while a response that keeps
// moving is never cut off even when the whole transfer outlasts one
// timeout window.

package httpserver

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
)

func startDeadlineServer(t *testing.T, handler http.Handler) net.Conn {
	t.Helper()
	transferBudget, err := budget.New(budget.Config{MaxUploads: 2, MaxDownloads: 2, MaxConnections: 4, TransferWaitTimeout: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	listener, server, err := Listen(ServerConfig{
		Bind:           "127.0.0.1",
		Port:           freePort(t),
		RequestTimeout: time.Second,
		TransferBudget: transferBudget,
		Handler:        handler,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveDone
	})
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestStalledRequestBodyIsCutAtTheRequestTimeout(t *testing.T) {
	conn := startDeadlineServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			// Returning without a response tears the connection down;
			// that is the observable the test pins.
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	if _, err := conn.Write([]byte("PUT /x HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nab")); err != nil {
		t.Fatal(err)
	}
	// The server must give up on the body after the one-second request
	// timeout and close the connection; a client-side read deadline of
	// five seconds would only fire if the server kept waiting.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 256)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			continue
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the server never cut the stalled body: %v", err)
		}
		if err == nil {
			t.Fatal("read returned no error and no data")
		}
		break
	}
}

func TestFlowingResponseIsNotCutByTheRequestTimeout(t *testing.T) {
	const chunks = 8
	conn := startDeadlineServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(chunks))
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for index := 0; index < chunks; index++ {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(300 * time.Millisecond)
		}
	}))
	if _, err := conn.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// The transfer runs 2.4 seconds in total, far past the one-second
	// request timeout: only a whole-request deadline would cut it.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	raw := make([]byte, 0, 512)
	buf := make([]byte, 256)
	for {
		n, err := conn.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			t.Fatalf("the flowing transfer was cut: %v (got %d bytes)", err, len(raw))
		}
		if index := indexOfBodyEnd(raw, chunks); index >= 0 {
			return
		}
	}
}

// indexOfBodyEnd reports whether raw already holds the full header block
// plus the declared number of body bytes.
func indexOfBodyEnd(raw []byte, chunks int) int {
	headerEnd := -1
	for index := 0; index+3 < len(raw); index++ {
		if raw[index] == '\r' && raw[index+1] == '\n' && raw[index+2] == '\r' && raw[index+3] == '\n' {
			headerEnd = index + 4
			break
		}
	}
	if headerEnd < 0 || len(raw)-headerEnd < chunks {
		return -1
	}
	return headerEnd + chunks
}
