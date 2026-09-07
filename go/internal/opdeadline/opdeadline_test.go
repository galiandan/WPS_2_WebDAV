// The opdeadline tests pin the socket-settimeout semantics on both
// directions: a stalled operation fails once the timeout lapses, and an
// exchange that keeps moving is never cut off no matter how long it runs.

package opdeadline

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func newPipe(t *testing.T, timeout time.Duration) (client *Conn, server *Conn) {
	t.Helper()
	rawClient, rawServer := net.Pipe()
	t.Cleanup(func() {
		rawClient.Close()
		rawServer.Close()
	})
	return Wrap(rawClient, timeout), Wrap(rawServer, timeout)
}

func TestStalledReadAndWriteFailAtTheDeadline(t *testing.T) {
	client, server := newPipe(t, 50*time.Millisecond)
	defer client.Close()
	defer server.Close()

	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("stalled read err = %v, want deadline exceeded", err)
	}
	// The failure must not poison the connection: once the peer answers,
	// the same connection still exchanges data.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := server.Write([]byte("x")); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("server write failed: %v", err)
		}
	}()
	buf := make([]byte, 1)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read after a timed-out read failed: %v", err)
	}
	if buf[0] != 'x' {
		t.Fatalf("read %q", buf)
	}
	<-done

	if _, err := server.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("stalled server read err = %v, want deadline exceeded", err)
	}
}

func TestStalledWriteFailsAtTheDeadline(t *testing.T) {
	// net.Pipe is unbuffered: a write with no reader blocks until the
	// deadline fires.
	client, server := newPipe(t, 50*time.Millisecond)
	defer client.Close()
	defer server.Close()

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := client.Write(bytes.Repeat([]byte("y"), 4096))
		done <- result{n, err}
	}()
	select {
	case got := <-done:
		if !errors.Is(got.err, os.ErrDeadlineExceeded) {
			t.Fatalf("stalled write err = %v, want deadline exceeded", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled write never returned")
	}
}

func TestFlowingExchangeOutlivesTheTimeout(t *testing.T) {
	client, server := newPipe(t, 200*time.Millisecond)
	defer client.Close()
	defer server.Close()

	// net.Pipe is synchronous, so an operation stays in flight until the
	// peer enters its counterpart. The echo goroutine therefore only
	// touches the pipe once the data is imminent, leaving the gaps between
	// exchanges — 300ms each, longer than one timeout window — completely
	// operation-free. A per-operation deadline must only govern operations
	// actually in flight, never those gaps.
	coming := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		for range coming {
			if _, err := io.ReadFull(server, buf); err != nil {
				return
			}
			if _, err := server.Write([]byte("z")); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 8; i++ {
		time.Sleep(300 * time.Millisecond)
		coming <- struct{}{}
		if _, err := client.Write([]byte("a")); err != nil {
			t.Fatalf("round %d write failed: %v", i, err)
		}
		buf := make([]byte, 1)
		if _, err := io.ReadFull(client, buf); err != nil {
			t.Fatalf("round %d read failed: %v", i, err)
		}
	}
}
