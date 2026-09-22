package remotefetch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type pipeHarness struct {
	t         *testing.T
	mu        sync.Mutex
	addresses []string
	requests  []*http.Request
	handler   func(int, *http.Request, net.Conn)
}

// TCP permits setting a deadline after the peer sends FIN. net.Pipe reports a
// closed-pipe error for that setter; normalize the test seam so Read can deliver
// EOF like a real socket with a completed close-delimited response.
type pipeDeadlineConn struct{ net.Conn }

func (c pipeDeadlineConn) SetReadDeadline(deadline time.Time) error {
	err := c.Conn.SetReadDeadline(deadline)
	if errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

func (h *pipeHarness) dial(ctx context.Context, network, address string) (net.Conn, error) {
	client, server := net.Pipe()
	h.mu.Lock()
	number := len(h.addresses)
	h.addresses = append(h.addresses, address)
	h.mu.Unlock()
	go func() {
		defer server.Close()
		request, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		defer request.Body.Close()
		h.mu.Lock()
		h.requests = append(h.requests, request)
		h.mu.Unlock()
		h.handler(number, request, server)
	}()
	return pipeDeadlineConn{client}, nil
}
func publicDNS(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}
func pipeClient(t *testing.T, handler func(int, *http.Request, net.Conn)) (*Client, *pipeHarness) {
	t.Helper()
	h := &pipeHarness{t: t, handler: handler}
	return NewClient(Config{LookupIP: publicDNS, DialContext: h.dial, TotalTimeout: 3 * time.Second, IdleTimeout: time.Second}), h
}
func response(conn net.Conn, status, headers, body string) {
	fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Length: %d\r\n%sConnection: close\r\n\r\n%s", status, len(body), headers, body)
}

func TestFetchPinsPublicDialPreservesHostAndReportsProgress(t *testing.T) {
	body := strings.Repeat("x", readBufferBytes+19)
	client, h := pipeClient(t, func(_ int, r *http.Request, conn net.Conn) {
		if r.Host != "download.example:8080" || r.URL.RawQuery != "signature=private-query" {
			t.Errorf("host or query changed")
		}
		if r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("unexpected request headers: %v", r.Header)
		}
		response(conn, "200 OK", "Content-Type: text/plain; charset=utf-8; name=private-query\r\n", body)
	})
	var output bytes.Buffer
	var progress []int64
	result, err := client.Fetch(context.Background(), "http://download.example:8080/file?signature=private-query", &output, int64(len(body)+1), func(n int64) { progress = append(progress, n) })
	if err != nil || result.Bytes != int64(len(body)) || output.String() != body || result.ContentType != "text/plain" {
		t.Fatalf("result=%+v err=%v bytes=%d", result, err, output.Len())
	}
	if len(h.addresses) != 1 || h.addresses[0] != "93.184.216.34:8080" {
		t.Fatal(h.addresses)
	}
	if len(progress) < 2 || progress[0] != 0 || progress[len(progress)-1] != result.Bytes {
		t.Fatal(progress)
	}
	for i := 1; i < len(progress); i++ {
		if progress[i] < progress[i-1] || progress[i] > result.Bytes {
			t.Fatal("invalid progress", progress)
		}
	}
}

func TestAddressPolicyRejectsSpecialRangesAndMappedPrivateIPv6(t *testing.T) {
	blocked := []string{"0.0.0.0", "0.1.2.3", "10.1.2.3", "100.64.0.1", "100.100.100.200", "100.127.255.254", "127.0.0.1", "169.254.169.254", "172.16.0.1", "172.31.255.255", "192.0.0.9", "192.0.2.1", "192.31.196.1", "192.52.193.1", "192.88.99.1", "192.168.2.1", "192.175.48.1", "198.18.0.1", "198.19.255.255", "198.51.100.1", "203.0.113.1", "224.0.0.1", "239.1.1.1", "240.0.0.1", "255.255.255.255", "168.63.129.16", "::", "::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::192.168.1.1", "64:ff9b::7f00:1", "64:ff9b:1::1", "fc00::1", "fd00:ec2::254", "fe80::1", "fec0::1", "ff02::1", "2001::1", "2001:20::1", "2001:db8::1", "2002:7f00:1::1", "2620:4f:8000::1", "3ffe::1", "3fff::1", "5f00::1"}
	for _, raw := range blocked {
		if publicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("accepted special IP %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "100.63.255.254", "100.128.0.1", "172.15.255.254", "172.32.0.1", "::ffff:8.8.8.8", "2001:4860:4860::8888", "2606:4700:4700::1111"} {
		if !publicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("rejected public IP %s", raw)
		}
	}
}

func TestForbiddenSourcesFailBeforeDial(t *testing.T) {
	for _, source := range []string{"file:///etc/passwd", "ftp://files.example/a", "http://user:secret@files.example/a", "http://127.0.0.1/a", "http://169.254.169.254/a", "http://[::ffff:127.0.0.1]/a", "http://[fe80::1%25eth0]/a", "http://files.example:0/a", "http://files.example:65536/a", "http://files.example/\nsecret", "http://files.example/" + strings.Repeat("x", MaxURLBytes)} {
		t.Run(source[:min(len(source), 60)], func(t *testing.T) {
			dialed := false
			client := NewClient(Config{LookupIP: publicDNS, DialContext: func(context.Context, string, string) (net.Conn, error) {
				dialed = true
				return nil, errors.New("unexpected")
			}})
			_, err := client.Fetch(context.Background(), source, io.Discard, 100, nil)
			if err == nil || dialed {
				t.Fatalf("forbidden source dialed=%t err=%v", dialed, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("URL leaked in error")
			}
		})
	}
}

func TestDNSRequiresEveryAnswerToBePublic(t *testing.T) {
	for _, answers := range [][]net.IPAddr{{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("127.0.0.1")}}, {{IP: net.ParseIP("::ffff:192.168.1.5")}}, {{IP: net.ParseIP("2001:4860::1"), Zone: "eth0"}}, {{IP: nil}}} {
		dialed := false
		client := NewClient(Config{LookupIP: func(context.Context, string) ([]net.IPAddr, error) { return answers, nil }, DialContext: func(context.Context, string, string) (net.Conn, error) { dialed = true; return nil, nil }})
		if _, err := client.Fetch(context.Background(), "http://files.example/a", io.Discard, 100, nil); !errors.Is(err, ErrForbiddenAddress) || dialed {
			t.Fatalf("err=%v dialed=%t", err, dialed)
		}
	}
}

func TestDNSRebindingCannotChangeTheDialAddress(t *testing.T) {
	client, h := pipeClient(t, func(_ int, _ *http.Request, conn net.Conn) { response(conn, "200 OK", "", "ok") })
	lookups := 0
	client.config.LookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return publicDNS(context.Background(), "")
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	if _, err := client.Fetch(context.Background(), "http://files.example/a", io.Discard, 10, nil); err != nil {
		t.Fatal(err)
	}
	if lookups != 1 || len(h.addresses) != 1 || h.addresses[0] != "93.184.216.34:80" {
		t.Fatalf("lookups=%d addresses=%v", lookups, h.addresses)
	}
}

func TestRedirectsRevalidateDNSAndDropSensitiveHeaders(t *testing.T) {
	t.Run("private redirect literal", func(t *testing.T) {
		client, h := pipeClient(t, func(_ int, _ *http.Request, conn net.Conn) {
			response(conn, "302 Found", "Location: http://169.254.169.254/metadata?secret=reflected\r\n", "")
		})
		_, err := client.Fetch(context.Background(), "http://public.example/a?secret=original", io.Discard, 100, nil)
		if !errors.Is(err, ErrForbiddenAddress) || len(h.addresses) != 1 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("err=%v addresses=%v", err, h.addresses)
		}
	})
	t.Run("same host rebound redirect", func(t *testing.T) {
		client, h := pipeClient(t, func(_ int, _ *http.Request, conn net.Conn) { response(conn, "302 Found", "Location: /second\r\n", "") })
		lookups := 0
		client.config.LookupIP = func(context.Context, string) ([]net.IPAddr, error) {
			lookups++
			if lookups == 1 {
				return publicDNS(context.Background(), "")
			}
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
		}
		if _, err := client.Fetch(context.Background(), "http://rebound.example/first", io.Discard, 100, nil); !errors.Is(err, ErrForbiddenAddress) || lookups != 2 || len(h.addresses) != 1 {
			t.Fatalf("err=%v lookups=%d addresses=%v", err, lookups, h.addresses)
		}
	})
	t.Run("public redirect has no referer cookies or auth", func(t *testing.T) {
		client, h := pipeClient(t, func(n int, r *http.Request, conn net.Conn) {
			if n == 0 {
				response(conn, "302 Found", "Location: http://other.example/final\r\nSet-Cookie: auth=must-not-propagate\r\n", "")
				return
			}
			if r.Header.Get("Referer") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
				t.Errorf("redirect propagated secrets: %v", r.Header)
			}
			response(conn, "200 OK", "", "done")
		})
		if _, err := client.Fetch(context.Background(), "http://first.example/start?signature=private", io.Discard, 100, nil); err != nil {
			t.Fatal(err)
		}
		if len(h.addresses) != 2 || h.requests[1].Host != "other.example" {
			t.Fatal(h.addresses)
		}
	})
	t.Run("redirect count", func(t *testing.T) {
		client, h := pipeClient(t, func(n int, _ *http.Request, conn net.Conn) {
			response(conn, "302 Found", fmt.Sprintf("Location: /hop-%d\r\n", n+1), "")
		})
		if _, err := client.Fetch(context.Background(), "http://loop.example/start", io.Discard, 100, nil); !errors.Is(err, ErrRedirects) || len(h.addresses) != MaxRedirects+1 {
			t.Fatalf("err=%v requests=%d", err, len(h.addresses))
		}
	})
}

func TestProxyEnvironmentIsBypassed(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9999")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9999")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:9999")
	client, h := pipeClient(t, func(_ int, r *http.Request, conn net.Conn) {
		if r.URL.IsAbs() {
			t.Error("request used proxy absolute form")
		}
		response(conn, "200 OK", "", "ok")
	})
	if _, err := client.Fetch(context.Background(), "http://public.example/file", io.Discard, 10, nil); err != nil {
		t.Fatal(err)
	}
	if len(h.addresses) != 1 || h.addresses[0] != "93.184.216.34:80" {
		t.Fatal(h.addresses)
	}
}

func TestFetchRejectsStatusEncodingShortBodyAndOverflow(t *testing.T) {
	cases := []struct {
		name, response string
		limit          int64
		want           error
		bytes          int
	}{
		{"status", "HTTP/1.1 403 Forbidden\r\nContent-Length: 30\r\nConnection: close\r\n\r\n<html>secret upstream</html>", 100, ErrStatus, 0},
		{"encoding", "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: 3\r\nConnection: close\r\n\r\nabc", 100, ErrEncoding, 0},
		{"duplicate encoding", "HTTP/1.1 200 OK\r\nContent-Encoding: identity\r\nContent-Encoding: gzip\r\nContent-Length: 3\r\nConnection: close\r\n\r\nabc", 100, ErrEncoding, 0},
		{"declared overflow", "HTTP/1.1 200 OK\r\nContent-Length: 999\r\nConnection: close\r\n\r\n", 10, ErrTooLarge, 0},
		{"short", "HTTP/1.1 200 OK\r\nContent-Length: 20\r\nConnection: close\r\n\r\nshort", 100, ErrIncomplete, 5},
		{"unknown overflow", "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\ntoo much data", 4, ErrTooLarge, 4},
		{"chunked overflow", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n8\r\nabcdefgh\r\n0\r\n\r\n", 4, ErrTooLarge, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := pipeClient(t, func(_ int, _ *http.Request, conn net.Conn) { io.WriteString(conn, tc.response) })
			var body bytes.Buffer
			result, err := client.Fetch(context.Background(), "http://source.example/file?signature=secret", &body, tc.limit, nil)
			if !errors.Is(err, tc.want) || body.Len() != tc.bytes || result.Bytes != int64(tc.bytes) {
				t.Fatalf("result=%+v err=%v body=%q", result, err, body.String())
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "html") || strings.Contains(err.Error(), "source.example") {
				t.Fatal("unredacted error", err)
			}
		})
	}
}

func TestChunkedAndExactLimitCompleteSuccessfully(t *testing.T) {
	for _, wire := range []string{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n4\r\ndata\r\n0\r\n\r\n", "HTTP/1.1 200 OK\r\nContent-Length: 4\r\nConnection: close\r\n\r\ndata"} {
		client, _ := pipeClient(t, func(_ int, _ *http.Request, conn net.Conn) { io.WriteString(conn, wire) })
		var output bytes.Buffer
		result, err := client.Fetch(context.Background(), "http://file.example/a", &output, 4, nil)
		if err != nil || result.Bytes != 4 || output.String() != "data" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
}

type reservingWriter struct {
	bytes.Buffer
	expected int64
	calls    int
	reject   bool
}

func (w *reservingWriter) SetExpectedSize(size int64) error {
	w.calls++
	w.expected = size
	if w.reject {
		return errors.New("private local disk path")
	}
	return nil
}
func TestExpectedSizeReservationRunsBeforeCopy(t *testing.T) {
	for _, known := range []bool{true, false} {
		client, _ := pipeClient(t, func(_ int, _ *http.Request, conn net.Conn) {
			header := ""
			if known {
				header = "Content-Length: 4\r\n"
			}
			fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\n%sConnection: close\r\n\r\ndata", header)
		})
		out := &reservingWriter{}
		if _, err := client.Fetch(context.Background(), "http://file.example/a", out, 8, nil); err != nil {
			t.Fatal(err)
		}
		want := int64(-1)
		if known {
			want = 4
		}
		if out.calls != 1 || out.expected != want || out.String() != "data" {
			t.Fatalf("writer=%+v", out)
		}
	}
	client, _ := pipeClient(t, func(_ int, _ *http.Request, conn net.Conn) { response(conn, "200 OK", "", "data") })
	out := &reservingWriter{reject: true}
	progress := false
	result, err := client.Fetch(context.Background(), "http://file.example/a", out, 10, func(int64) { progress = true })
	if !errors.Is(err, ErrReservation) || result.Bytes != 0 || out.Len() != 0 || progress || strings.Contains(err.Error(), "private") {
		t.Fatalf("result=%+v err=%v writer=%+v progress=%t", result, err, out, progress)
	}
}

func TestFetchCancellationClosesBlockedBody(t *testing.T) {
	started, closed := make(chan struct{}), make(chan struct{})
	client, _ := pipeClient(t, func(_ int, _ *http.Request, conn net.Conn) {
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\nConnection: close\r\n\r\n")
		close(started)
		io.Copy(io.Discard, conn)
		close(closed)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.Fetch(ctx, "http://file.example/a?token=secret", io.Discard, 20, nil)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop fetch")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("source connection leaked")
	}
}

func TestTimeoutsAndDNSDialErrorsAreRedacted(t *testing.T) {
	client, _ := pipeClient(t, func(_ int, _ *http.Request, conn net.Conn) {
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\nConnection: close\r\n\r\n")
		io.Copy(io.Discard, conn)
	})
	client.config.IdleTimeout = 20 * time.Millisecond
	if _, err := client.Fetch(context.Background(), "http://file.example/a?secret=secret", io.Discard, 20, nil); !errors.Is(err, ErrTimeout) {
		t.Fatal(err)
	}
	client = NewClient(Config{DNSTimeout: 20 * time.Millisecond, LookupIP: func(ctx context.Context, _ string) ([]net.IPAddr, error) { <-ctx.Done(); return nil, ctx.Err() }})
	if _, err := client.Fetch(context.Background(), "http://file.example/a?secret=secret", io.Discard, 20, nil); !errors.Is(err, ErrTimeout) {
		t.Fatal(err)
	}
	client = NewClient(Config{LookupIP: func(context.Context, string) ([]net.IPAddr, error) {
		return nil, errors.New("resolver leaked secret URL")
	}})
	if _, err := client.Fetch(context.Background(), "http://file.example/a", io.Discard, 20, nil); !errors.Is(err, ErrDNS) || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
	client = NewClient(Config{LookupIP: publicDNS, DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial leaked secret URL")
	}})
	if _, err := client.Fetch(context.Background(), "http://file.example/a", io.Discard, 20, nil); !errors.Is(err, ErrNetwork) || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}

func TestTLSVerificationIsNotDisabled(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"file.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	serverNames := make(chan string, 1)
	// A self-signed certificate for the correct hostname must fail trust
	// verification. Skipping certificate verification would make this succeed.
	client := NewClient(Config{LookupIP: publicDNS, DialContext: func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			server := tls.Server(b, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				serverNames <- hello.ServerName
				return nil, nil
			}})
			if server.Handshake() == nil {
				if _, err := http.ReadRequest(bufio.NewReader(server)); err == nil {
					response(server, "200 OK", "", "untrusted")
				}
			}
		}()
		return pipeDeadlineConn{a}, nil
	}, TotalTimeout: time.Second})
	if _, err := client.Fetch(context.Background(), "https://file.example/a", io.Discard, 20, nil); err == nil {
		t.Fatal("TLS accepted an untrusted certificate")
	}
	select {
	case name := <-serverNames:
		if name != "file.example" {
			t.Fatalf("TLS used pinned IP as SNI: %q", name)
		}
	case <-time.After(time.Second):
		t.Fatal("TLS handshake did not begin")
	}
}
