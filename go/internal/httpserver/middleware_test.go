package httpserver

import (
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
)

// routingRecorder plays the router in chain tests and records every arrival.
type routingRecorder struct {
	mu    sync.Mutex
	calls int
}

func (rec *routingRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec.mu.Lock()
	rec.calls++
	rec.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (rec *routingRecorder) callCount() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.calls
}

type chainHarness struct {
	handler   http.Handler
	router    *routingRecorder
	healthHit *int
	logLines  []string
	panics    []any
}

func newChainHarness(t *testing.T, mutate func(*ChainConfig)) *chainHarness {
	t.Helper()
	router := &routingRecorder{}
	healthHits := 0
	harness := &chainHarness{router: router, healthHit: &healthHits}
	config := ChainConfig{
		Router: router,
		Health: func(w http.ResponseWriter, r *http.Request) {
			*harness.healthHit++
			w.WriteHeader(http.StatusOK)
		},
		Auth: BasicAuthConfig{Username: "user", Password: "pass"},
		Log: func(requestID, method, path string) {
			harness.logLines = append(harness.logLines, requestID+" "+method+" "+path)
		},
		PanicLog: func(recovered any, stack []byte) {
			harness.panics = append(harness.panics, recovered)
		},
		NewRequestID: func() string { return "fixed-id" },
	}
	if mutate != nil {
		mutate(&config)
	}
	handler, err := NewChain(config)
	if err != nil {
		t.Fatal(err)
	}
	harness.handler = handler
	return harness
}

func basicCredentials(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

func serveChain(harness *chainHarness, method, target string, headers map[string]string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	harness.handler.ServeHTTP(recorder, request)
	return recorder
}

// TestMiddlewareOrderKeepsProtectedRequestsFromRouting is the B601
// completion gate: unauthenticated, cross-origin, oversized, and
// boundary-invalid requests never reach the router, and the precedence
// between the guards mirrors the Python dispatch order.
func TestMiddlewareOrderKeepsProtectedRequestsFromRouting(t *testing.T) {
	t.Run("unauthenticated request does not route", func(t *testing.T) {
		harness := newChainHarness(t, nil)
		recorder := serveChain(harness, "GET", "/dav/file.txt", nil, "")
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
		if harness.router.callCount() != 0 {
			t.Error("router was reached without credentials")
		}
	})

	t.Run("framing violation beats authentication", func(t *testing.T) {
		harness := newChainHarness(t, nil)
		recorder := serveChain(harness, "GET", "/dav/file.txt",
			map[string]string{"Content-Length": "5"}, "hello")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", recorder.Code)
		}
		if body := recorder.Body.String(); body != "request body is not supported for this method\n" {
			t.Errorf("body = %q", body)
		}
		if harness.router.callCount() != 0 {
			t.Error("router was reached with a forbidden body")
		}
	})

	t.Run("unknown method beats authentication", func(t *testing.T) {
		harness := newChainHarness(t, nil)
		recorder := serveChain(harness, "TRACE", "/dav/", nil, "")
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", recorder.Code)
		}
		if harness.router.callCount() != 0 {
			t.Error("router was reached with an unknown method")
		}
	})

	t.Run("authentication beats origin check", func(t *testing.T) {
		harness := newChainHarness(t, nil)
		recorder := serveChain(harness, "PUT", "/dav/file.txt",
			map[string]string{
				"Authorization": basicCredentials("user", "wrong"),
				"Origin":        "http://evil.example",
			}, "")
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
		if harness.router.callCount() != 0 {
			t.Error("router was reached with bad credentials")
		}
	})

	t.Run("cross-origin mutation does not route", func(t *testing.T) {
		harness := newChainHarness(t, nil)
		recorder := serveChain(harness, "PUT", "/dav/file.txt",
			map[string]string{
				"Authorization": basicCredentials("user", "pass"),
				"Origin":        "http://evil.example",
			}, "")
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", recorder.Code)
		}
		if body := recorder.Body.String(); body != "cross-origin mutation is not allowed\n" {
			t.Errorf("body = %q", body)
		}
		if harness.router.callCount() != 0 {
			t.Error("router was reached from a cross-origin mutation")
		}
	})

	t.Run("health precedes authentication and skips the router", func(t *testing.T) {
		harness := newChainHarness(t, nil)
		recorder := serveChain(harness, "GET", "/healthz", nil, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		if *harness.healthHit != 1 {
			t.Error("health handler was not served before auth")
		}
		if harness.router.callCount() != 0 {
			t.Error("health request reached the router")
		}
	})

	t.Run("authorized same-origin request routes", func(t *testing.T) {
		harness := newChainHarness(t, nil)
		recorder := serveChain(harness, "PUT", "/dav/file.txt",
			map[string]string{
				"Authorization": basicCredentials("user", "pass"),
				"Origin":        "http://example.com",
			}, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		if harness.router.callCount() != 1 {
			t.Errorf("router calls = %d, want 1", harness.router.callCount())
		}
	})

	t.Run("safe methods skip the origin check", func(t *testing.T) {
		harness := newChainHarness(t, nil)
		recorder := serveChain(harness, "GET", "/dav/file.txt",
			map[string]string{
				"Authorization": basicCredentials("user", "pass"),
				"Origin":        "http://evil.example",
			}, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		if harness.router.callCount() != 1 {
			t.Errorf("router calls = %d, want 1", harness.router.callCount())
		}
	})
}

// TestBoundaryFramingOnLiveServer runs the framing rules through the real
// transport, where Go has already decoded chunked bodies.
func TestBoundaryFramingOnLiveServer(t *testing.T) {
	harness := newChainHarness(t, nil)
	server := httptest.NewServer(harness.handler)
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")

	exchange := func(request string) string {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte(request)); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(conn)
		if err != nil {
			t.Fatalf("reading response: %v", err)
		}
		return string(data)
	}

	// Mirrors tests/test_server.py::test_invalid_request_framing_closes_the_connection.
	response := exchange("GET /dav/ HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n")
	if !strings.Contains(response, "400") || !strings.Contains(response, "Connection: close") {
		t.Errorf("chunked framing response: %s", response)
	}
	if !strings.Contains(response, "Transfer-Encoding is not supported") {
		t.Errorf("chunked framing body missing: %s", response)
	}

	response = exchange("GET /dav/x HTTP/1.1\r\nHost: x\r\nConnection: close\r\nContent-Length: 5\r\n\r\nhello")
	if !strings.Contains(response, "400") || !strings.Contains(response, "request body is not supported for this method") {
		t.Errorf("GET with body response: %s", response)
	}

	response = exchange("GET /dav/x HTTP/1.1\r\nHost: x\r\nConnection: close\r\nAuthorization: " + basicCredentials("user", "pass") + "\r\nContent-Length: 0\r\n\r\n")
	if !strings.Contains(response, "200") {
		t.Errorf("GET with empty body must route: %s", response)
	}
	if harness.router.callCount() != 1 {
		t.Errorf("router calls = %d, want 1", harness.router.callCount())
	}

	// Documented transport deviation: Go rejects duplicate Content-Length
	// before any handler runs; Python's boundary produced the same status
	// with its own message.
	response = exchange("GET /dav/x HTTP/1.1\r\nHost: x\r\nConnection: close\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n")
	if !strings.Contains(response, "400") {
		t.Errorf("duplicate Content-Length response: %s", response)
	}
}

// TestBasicAuthCoversHalfConfiguredAndHotFiles pins the credential rules:
// D-05 half-configuration rejects everything, file credentials hot-reload
// per request, and read failures behave like empty credentials.
func TestBasicAuthCoversHalfConfiguredAndHotFiles(t *testing.T) {
	passwordFile := filepath.Join(t.TempDir(), "adapter-password")
	if err := os.WriteFile(passwordFile, []byte("pass-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	harness := newChainHarness(t, func(config *ChainConfig) {
		config.Auth = BasicAuthConfig{
			Username:     "user",
			PasswordFile: passwordFile,
			ReadSecret: func(path string) (string, error) {
				data, err := os.ReadFile(path)
				if err != nil {
					return "", err
				}
				return strings.TrimSpace(string(data)), nil
			},
		}
	})

	recorder := serveChain(harness, "GET", "/dav/x",
		map[string]string{"Authorization": basicCredentials("user", "pass-one")}, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status with file password = %d", recorder.Code)
	}

	// Hot reload: the next request sees the new file content.
	if err := os.WriteFile(passwordFile, []byte("pass-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder = serveChain(harness, "GET", "/dav/x",
		map[string]string{"Authorization": basicCredentials("user", "pass-one")}, "")
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("old password still accepted: %d", recorder.Code)
	}
	recorder = serveChain(harness, "GET", "/dav/x",
		map[string]string{"Authorization": basicCredentials("user", "pass-two")}, "")
	if recorder.Code != http.StatusOK {
		t.Errorf("new password rejected: %d", recorder.Code)
	}

	// Read failure behaves like an empty credential.
	harness = newChainHarness(t, func(config *ChainConfig) {
		config.Auth = BasicAuthConfig{
			Username:     "user",
			PasswordFile: filepath.Join(t.TempDir(), "missing"),
			ReadSecret: func(string) (string, error) {
				return "", os.ErrNotExist
			},
		}
	})
	recorder = serveChain(harness, "GET", "/dav/x",
		map[string]string{"Authorization": basicCredentials("user", "anything")}, "")
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("failed read must reject: %d", recorder.Code)
	}

	// D-05: username only enables auth and rejects every request.
	harness = newChainHarness(t, func(config *ChainConfig) {
		config.Auth = BasicAuthConfig{Username: "user"}
	})
	recorder = serveChain(harness, "GET", "/dav/x",
		map[string]string{"Authorization": basicCredentials("user", "")}, "")
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("half-configured auth must reject: %d", recorder.Code)
	}
}

// TestBasicAuthRejectsMalformedHeaders pins the Authorization parsing rules
// (strict base64 alphabet, required colon, Basic scheme case-insensitive).
func TestBasicAuthRejectsMalformedHeaders(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"valid", basicCredentials("user", "pass"), http.StatusOK},
		{"scheme case", "bAsIc " + base64.StdEncoding.EncodeToString([]byte("user:pass")), http.StatusOK},
		{"no scheme", base64.StdEncoding.EncodeToString([]byte("user:pass")), http.StatusUnauthorized},
		{"wrong scheme", "Bearer xyz", http.StatusUnauthorized},
		{"no colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("userpass")), http.StatusUnauthorized},
		{"invalid alphabet", "Basic !!!not-base64!!!", http.StatusUnauthorized},
		{"wrong user", basicCredentials("who", "pass"), http.StatusUnauthorized},
		{"wrong password", basicCredentials("user", "nope"), http.StatusUnauthorized},
		{"empty header", "Basic ", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newChainHarness(t, nil)
			recorder := serveChain(harness, "GET", "/dav/x",
				map[string]string{"Authorization": tc.header}, "")
			if recorder.Code != tc.want {
				t.Errorf("status = %d, want %d", recorder.Code, tc.want)
			}
		})
	}
}

// TestMutationOriginRules pins the Origin/Referer decision table.
func TestMutationOriginRules(t *testing.T) {
	auth := map[string]string{"Authorization": basicCredentials("user", "pass")}
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"no headers allowed", "PUT", auth, http.StatusOK},
		{"same host origin", "PUT", merge(auth, map[string]string{"Origin": "http://example.com"}), http.StatusOK},
		{"same host with port", "PUT", merge(auth, map[string]string{"Origin": "http://example.com:80"}), http.StatusOK},
		{"origin may omit port against proxy host", "PUT", merge(auth, map[string]string{"Origin": "http://example.com"}), http.StatusOK},
		{"trailing dot matches", "PUT", merge(auth, map[string]string{"Origin": "http://example.com."}), http.StatusOK},
		{"host case matches", "PUT", merge(auth, map[string]string{"Origin": "http://EXAMPLE.com"}), http.StatusOK},
		{"https scheme matches", "PUT", merge(auth, map[string]string{"Origin": "https://example.com"}), http.StatusOK},
		{"other host", "PUT", merge(auth, map[string]string{"Origin": "http://evil.example"}), http.StatusForbidden},
		{"origin with path", "PUT", merge(auth, map[string]string{"Origin": "http://example.com/x"}), http.StatusForbidden},
		{"origin with query", "PUT", merge(auth, map[string]string{"Origin": "http://example.com?x=1"}), http.StatusForbidden},
		{"origin with fragment", "PUT", merge(auth, map[string]string{"Origin": "http://example.com#f"}), http.StatusForbidden},
		{"origin with userinfo", "PUT", merge(auth, map[string]string{"Origin": "http://u@example.com"}), http.StatusForbidden},
		{"origin control char", "PUT", merge(auth, map[string]string{"Origin": "http://example.com\nx"}), http.StatusForbidden},
		{"empty origin value", "PUT", merge(auth, map[string]string{"Origin": ""}), http.StatusForbidden},
		{"non-http scheme", "PUT", merge(auth, map[string]string{"Origin": "ftp://example.com"}), http.StatusForbidden},
		{"referer allowed with path", "PUT", merge(auth, map[string]string{"Referer": "http://example.com/page"}), http.StatusOK},
		{"referer other host", "PUT", merge(auth, map[string]string{"Referer": "http://evil.example/page"}), http.StatusForbidden},
		{"origin wins over referer", "PUT", merge(auth, map[string]string{"Origin": "http://evil.example", "Referer": "http://example.com/x"}), http.StatusForbidden},
		{"origin wins good referer bad", "PUT", merge(auth, map[string]string{"Origin": "http://example.com", "Referer": "http://evil.example"}), http.StatusOK},
		{"get ignores cross origin", "GET", merge(auth, map[string]string{"Origin": "http://evil.example"}), http.StatusOK},
		{"propfind ignores cross origin", "PROPFIND", merge(auth, map[string]string{"Origin": "http://evil.example"}), http.StatusOK},
		{"mkcol guarded", "MKCOL", merge(auth, map[string]string{"Origin": "http://evil.example"}), http.StatusForbidden},
		{"lock guarded", "LOCK", merge(auth, map[string]string{"Origin": "http://evil.example"}), http.StatusForbidden},
		{"unlock guarded", "UNLOCK", merge(auth, map[string]string{"Origin": "http://evil.example"}), http.StatusForbidden},
		{"copy guarded", "COPY", merge(auth, map[string]string{"Origin": "http://evil.example"}), http.StatusForbidden},
		{"move guarded", "MOVE", merge(auth, map[string]string{"Origin": "http://evil.example"}), http.StatusForbidden},
		{"patch guarded", "PATCH", merge(auth, map[string]string{"Origin": "http://evil.example"}), http.StatusForbidden},
		{"delete guarded", "DELETE", merge(auth, map[string]string{"Origin": "http://evil.example"}), http.StatusForbidden},
		{"post guarded", "POST", merge(auth, map[string]string{"Origin": "http://evil.example"}), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newChainHarness(t, nil)
			recorder := serveChain(harness, tc.method, "/dav/file.txt", tc.headers, "")
			if recorder.Code != tc.want {
				t.Errorf("status = %d, want %d", recorder.Code, tc.want)
			}
		})
	}
}

func merge(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

// TestPanicRecoveryReturnsFixed500 pins the recovery contract: fixed body,
// no stack echo, panic visible only in the server-side sink.
func TestPanicRecoveryReturnsFixed500(t *testing.T) {
	harness := newChainHarness(t, func(config *ChainConfig) {
		config.Router = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("secret route state /dav/x")
		})
	})
	recorder := serveChain(harness, "GET", "/dav/x",
		map[string]string{"Authorization": basicCredentials("user", "pass")}, "")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != "internal server error\n" {
		t.Errorf("body = %q", body)
	}
	if strings.Contains(recorder.Body.String(), "secret route state") || strings.Contains(recorder.Body.String(), "goroutine") {
		t.Error("panic details leaked into the response")
	}
	if len(harness.panics) != 1 {
		t.Fatalf("panic log calls = %d, want 1", len(harness.panics))
	}
	if !strings.Contains(harness.panics[0].(string), "secret route state") {
		t.Errorf("panic sink missed the value: %v", harness.panics[0])
	}
}

// TestSecurityLogRedactsQuery pins the access-log redaction: method plus
// query-free path, control characters replaced, request ID attached.
func TestSecurityLogRedactsQuery(t *testing.T) {
	harness := newChainHarness(t, nil)
	serveChain(harness, "GET", "/dav/a%20b?token=secret&path=%2Fx", nil, "")
	serveChain(harness, "PUT", "/api/v1/entries?path=%2Fhello.txt",
		map[string]string{
			"Authorization": basicCredentials("user", "pass"),
			"Origin":        "http://example.com",
		}, "")
	if len(harness.logLines) != 2 {
		t.Fatalf("log lines = %v", harness.logLines)
	}
	if harness.logLines[0] != "fixed-id GET /dav/a%20b" {
		t.Errorf("first line = %q", harness.logLines[0])
	}
	if harness.logLines[1] != "fixed-id PUT /api/v1/entries" {
		t.Errorf("second line = %q", harness.logLines[1])
	}
}

// TestRequestIDExposedToHandlers keeps the correlation ID reachable.
func TestRequestIDExposedToHandlers(t *testing.T) {
	config := ChainConfig{
		Router: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if RequestIDFrom(r.Context()) != "gen-1" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}),
		Health:       func(w http.ResponseWriter, r *http.Request) {},
		NewRequestID: func() string { return "gen-1" },
	}
	handler, err := NewChain(config)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/dav/x", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("request ID not visible to handlers: %d", recorder.Code)
	}
}

// TestNewChainValidation keeps misassemblies at construction time.
func TestNewChainValidation(t *testing.T) {
	if _, err := NewChain(ChainConfig{Health: func(w http.ResponseWriter, r *http.Request) {}}); err == nil {
		t.Error("missing router must fail construction")
	}
	if _, err := NewChain(ChainConfig{Router: http.NotFoundHandler()}); err == nil {
		t.Error("missing health handler must fail construction")
	}
	if _, err := NewChain(ChainConfig{
		Router: http.NotFoundHandler(),
		Health: func(w http.ResponseWriter, r *http.Request) {},
		Auth:   BasicAuthConfig{UsernameFile: "/x"},
	}); err == nil {
		t.Error("credential file without reader must fail construction")
	}
}

// freePort asks the OS for an unused TCP port, mirroring the Python test
// harness.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// TestListenValidation mirrors create_server's argument checks.
func TestListenValidation(t *testing.T) {
	transferBudget := newTestBudget(t)
	if _, _, err := Listen(ServerConfig{Bind: "127.0.0.1", Port: 0, RequestTimeout: time.Second, TransferBudget: transferBudget, Handler: http.NotFoundHandler()}); err == nil || err.Error() != "port must be between 1 and 65535" {
		t.Errorf("port 0 error = %v", err)
	}
	if _, _, err := Listen(ServerConfig{Bind: "127.0.0.1", Port: 65536, RequestTimeout: time.Second, TransferBudget: transferBudget, Handler: http.NotFoundHandler()}); err == nil {
		t.Error("port above range must fail")
	}
	if _, _, err := Listen(ServerConfig{Bind: "127.0.0.1", Port: 1, RequestTimeout: 0, TransferBudget: transferBudget, Handler: http.NotFoundHandler()}); err == nil || err.Error() != "request_timeout must be positive" {
		t.Errorf("timeout error = %v", err)
	}
	if _, _, err := Listen(ServerConfig{Bind: "127.0.0.1", Port: 1, RequestTimeout: time.Second, Handler: http.NotFoundHandler()}); err == nil {
		t.Error("missing budget must fail")
	}
	if _, _, err := Listen(ServerConfig{Bind: "127.0.0.1", Port: 1, RequestTimeout: time.Second, TransferBudget: transferBudget}); err == nil {
		t.Error("missing handler must fail")
	}
}

func newTestBudget(t *testing.T) *budget.Budget {
	t.Helper()
	transferBudget, err := budget.New(budget.Config{MaxUploads: 1, MaxDownloads: 1, MaxConnections: 2, TransferWaitTimeout: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	return transferBudget
}

// TestConnectionSlotsGateAccepts pins D-09: over-limit connections are
// closed at accept without a slot, and slots free up when connections close.
func TestConnectionSlotsGateAccepts(t *testing.T) {
	transferBudget := newTestBudget(t)
	port := freePort(t)
	listener, server, err := Listen(ServerConfig{
		Bind:           "127.0.0.1",
		Port:           port,
		RequestTimeout: 5 * time.Second,
		TransferBudget: transferBudget,
		Handler:        http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(listener)
	}()
	defer func() {
		_ = server.Close()
		<-serveDone
	}()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	hold := func() net.Conn {
		t.Helper()
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		return conn
	}

	first := hold()
	defer first.Close()
	second := hold()
	defer second.Close()

	// Third connection: the listener closes it before any exchange.
	third, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	reject := make(chan error, 1)
	go func() {
		// A refused connection typically surfaces as ECONNRESET on the
		// client write or a clean EOF on read; both mean "closed without
		// a response". Only visible response data would be wrong.
		if _, err := third.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			reject <- nil
			return
		}
		buf := make([]byte, 64)
		n, err := third.Read(buf)
		if n > 0 {
			reject <- errors.New("unexpected response data")
			return
		}
		if err == nil {
			reject <- errors.New("connection stayed open without data")
			return
		}
		reject <- nil
	}()
	select {
	case err := <-reject:
		if err != nil {
			t.Fatalf("third connection: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("third connection neither closed nor answered")
	}
	third.Close()

	// Slots free when held connections close.
	_ = first.Close()
	_ = second.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if transferBudget.Stats().ConnectionsActive == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection slots never freed: %d active", transferBudget.Stats().ConnectionsActive)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
