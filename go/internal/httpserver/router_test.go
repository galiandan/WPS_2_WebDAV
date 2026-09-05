package httpserver

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// recordingHandlers records which route the router selected and what it
// extracted from the raw request target.
type recordingHandlers struct {
	healthCalls int
	webAppCalls int
	assetNames  []string
	restRoutes  []RESTRoute
	davPaths    []string
}

func (h *recordingHandlers) toConfig() Handlers {
	return Handlers{
		Health: func(w http.ResponseWriter, r *http.Request) {
			h.healthCalls++
		},
		WebApp: func(w http.ResponseWriter, r *http.Request) {
			h.webAppCalls++
		},
		WebAsset: func(w http.ResponseWriter, r *http.Request, name string) {
			h.assetNames = append(h.assetNames, name)
		},
		REST: func(w http.ResponseWriter, r *http.Request, route RESTRoute) {
			h.restRoutes = append(h.restRoutes, route)
		},
		DAV: func(w http.ResponseWriter, r *http.Request, davPath string) {
			h.davPaths = append(h.davPaths, davPath)
		},
	}
}

func newTestRouter(t *testing.T, handlers *recordingHandlers, davPrefix, restPrefix string) *Router {
	t.Helper()
	router, err := NewRouter(RouterConfig{
		DAVPrefix:  davPrefix,
		RESTPrefix: restPrefix,
		Handlers:   handlers.toConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func serveRoute(t *testing.T, router *Router, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest(method, target))
	return recorder
}

// TestNewRouterRequiresHandlers keeps the wiring honest: a route selected
// with a missing handler would otherwise panic per request.
func TestNewRouterRequiresHandlers(t *testing.T) {
	config := RouterConfig{DAVPrefix: "/dav", RESTPrefix: "/api/v1", Handlers: Handlers{}}
	if _, err := NewRouter(config); err == nil {
		t.Fatal("expected an error for missing handlers")
	}
	full := recordingHandlers{}
	if _, err := NewRouter(RouterConfig{Handlers: full.toConfig()}); err != nil {
		t.Fatalf("complete handlers rejected: %v", err)
	}
}

// TestNormalizePrefix pins AdapterApplication._normalise_prefix.
func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"":          "/",
		"/":         "/",
		"dav":       "/dav",
		"/dav":      "/dav",
		"/dav/":     "/dav",
		"///":       "/",
		"/dav///":   "/dav",
		"/api/v1/":  "/api/v1",
		"/wps-dav/": "/wps-dav",
	}
	for input, want := range cases {
		if got := normalizePrefix(input); got != want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestRouteTable is the B600 completion gate: trailing slash, encoded slash,
// custom prefix, and unknown routes, with the per-method route order mirroring
// the Python do_* handlers.
func TestRouteTable(t *testing.T) {
	assetName := func(name string) *string { return &name }
	cases := []struct {
		name       string
		method     string
		target     string
		wantStatus int
		wantHealth int
		wantWebApp int
		wantAsset  *string
		wantREST   []RESTRoute
		wantDAV    []string
	}{
		// Health: exact raw-path match on GET only, query allowed.
		{"health", "GET", "/healthz", 200, 1, 0, nil, nil, nil},
		{"health with query", "GET", "/healthz?probe=1", 200, 1, 0, nil, nil, nil},
		{"health trailing slash is unknown", "GET", "/healthz/", 404, 0, 0, nil, nil, nil},

		// Web app: three fixed entries, raw-path match.
		{"web root", "GET", "/", 200, 0, 1, nil, nil, nil},
		{"web alias", "GET", "/web", 200, 0, 1, nil, nil, nil},
		{"web alias slash", "GET", "/web/", 200, 0, 1, nil, nil, nil},
		{"encoded web slash misses the raw match", "GET", "/web%2F", 404, 0, 0, nil, nil, nil},

		// Assets: GET and HEAD, name stays percent-encoded.
		{"asset", "GET", "/assets/app.js", 200, 0, 0, assetName("app.js"), nil, nil},
		{"asset head", "HEAD", "/assets/style.css", 200, 0, 0, assetName("style.css"), nil, nil},
		{"asset root name is empty", "GET", "/assets/", 200, 0, 0, assetName(""), nil, nil},
		{"asset nested name keeps slashes", "GET", "/assets/deep/x.js", 200, 0, 0, assetName("deep/x.js"), nil, nil},
		{"asset prefix without slash is unknown", "GET", "/assets", 404, 0, 0, nil, nil, nil},

		// REST: prefix match on the raw path, suffix trimmed.
		{"rest bare", "GET", "/api/v1", 200, 0, 0, nil, []RESTRoute{{Suffix: "", Query: url.Values{}}}, nil},
		{"rest trailing slash", "GET", "/api/v1/", 200, 0, 0, nil, []RESTRoute{{Suffix: "", Query: url.Values{}}}, nil},
		{"rest inner slash collapses", "GET", "/api/v1//metadata", 200, 0, 0, nil,
			[]RESTRoute{{Suffix: "metadata", Query: url.Values{}}}, nil},
		{"rest trailing slash on route", "GET", "/api/v1/metadata/", 200, 0, 0, nil,
			[]RESTRoute{{Suffix: "metadata", Query: url.Values{}}}, nil},
		{"rest query decodes once", "GET", "/api/v1/metadata?path=%2Fa+b", 200, 0, 0, nil,
			[]RESTRoute{{Suffix: "metadata", Query: url.Values{"path": {"/a b"}}}}, nil},
		{"rest session import", "POST", "/api/v1/session", 200, 0, 0, nil,
			[]RESTRoute{{Suffix: "session", Query: url.Values{}}}, nil},
		{"rest patch", "PATCH", "/api/v1/entries?path=%2Fx", 200, 0, 0, nil,
			[]RESTRoute{{Suffix: "entries", Query: url.Values{"path": {"/x"}}}}, nil},
		{"rest put", "PUT", "/api/v1/entries?path=%2Fx", 200, 0, 0, nil,
			[]RESTRoute{{Suffix: "entries", Query: url.Values{"path": {"/x"}}}}, nil},
		{"rest delete", "DELETE", "/api/v1/entries?path=%2Fx", 200, 0, 0, nil,
			[]RESTRoute{{Suffix: "entries", Query: url.Values{"path": {"/x"}}}}, nil},

		// DAV: prefix match on the raw path, business path decoded once.
		{"dav root no slash", "PROPFIND", "/dav", 200, 0, 0, nil, nil, []string{"/"}},
		{"dav root trailing slash", "PROPFIND", "/dav/", 200, 0, 0, nil, nil, []string{"/"}},
		{"dav trailing slash is preserved", "PROPFIND", "/dav/docs/", 200, 0, 0, nil, nil, []string{"/docs/"}},
		{"dav encoded space", "GET", "/dav/a%20b", 200, 0, 0, nil, nil, []string{"/a b"}},
		{"dav encoded slash splits", "GET", "/dav/a%2Fb", 200, 0, 0, nil, nil, []string{"/a/b"}},
		{"dav double-encoded stays literal", "GET", "/dav/a%252Fb", 200, 0, 0, nil, nil, []string{"/a%2Fb"}},
		{"dav invalid utf-8 reaches the handler", "GET", "/dav/%FF", 200, 0, 0, nil, nil, []string{"/\xff"}},
		{"dav head", "HEAD", "/dav/file.txt", 200, 0, 0, nil, nil, []string{"/file.txt"}},
		{"dav delete", "DELETE", "/dav/file.txt", 200, 0, 0, nil, nil, []string{"/file.txt"}},
		{"dav mkcol", "MKCOL", "/dav/new-dir", 200, 0, 0, nil, nil, []string{"/new-dir"}},
		{"dav lock", "LOCK", "/dav/file.txt", 200, 0, 0, nil, nil, []string{"/file.txt"}},
		{"dav copy", "COPY", "/dav/file.txt", 200, 0, 0, nil, nil, []string{"/file.txt"}},

		// Encoded prefixes never match (Python matches the raw target).
		{"encoded dav prefix is unknown", "GET", "/dav%2Fx", 404, 0, 0, nil, nil, nil},
		{"encoded rest prefix is unknown", "GET", "/api%2Fv1%2Fmetadata", 404, 0, 0, nil, nil, nil},
		{"dav prefix as rest suffix is unknown", "PROPFIND", "/api/v1/metadata", 404, 0, 0, nil, nil, nil},

		// Unknown routes: text 404, connection close (handled below).
		{"unknown path get", "GET", "/nope", 404, 0, 0, nil, nil, nil},
		{"unknown path post", "POST", "/nope", 404, 0, 0, nil, nil, nil},
		{"unknown path head", "HEAD", "/nope", 404, 0, 0, nil, nil, nil},
		{"web page is not served via head", "HEAD", "/web", 404, 0, 0, nil, nil, nil},
		{"rest is not served via head", "HEAD", "/api/v1/metadata", 404, 0, 0, nil, nil, nil},
		{"post is rest-only", "POST", "/dav/x", 404, 0, 0, nil, nil, nil},
		{"patch outside rest is 501", "PATCH", "/dav/x", 501, 0, 0, nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handlers := &recordingHandlers{}
			router := newTestRouter(t, handlers, "/dav", "/api/v1")
			recorder := serveRoute(t, router, tc.method, tc.target)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if handlers.healthCalls != tc.wantHealth {
				t.Errorf("health calls = %d, want %d", handlers.healthCalls, tc.wantHealth)
			}
			if handlers.webAppCalls != tc.wantWebApp {
				t.Errorf("web app calls = %d, want %d", handlers.webAppCalls, tc.wantWebApp)
			}
			if tc.wantAsset == nil {
				if len(handlers.assetNames) != 0 {
					t.Errorf("unexpected asset dispatch %v", handlers.assetNames)
				}
			} else {
				if len(handlers.assetNames) != 1 || handlers.assetNames[0] != *tc.wantAsset {
					t.Errorf("asset names = %v, want [%q]", handlers.assetNames, *tc.wantAsset)
				}
			}
			if len(handlers.restRoutes) != len(tc.wantREST) {
				t.Fatalf("rest routes = %v, want %v", handlers.restRoutes, tc.wantREST)
			}
			for i, want := range tc.wantREST {
				if handlers.restRoutes[i].Suffix != want.Suffix {
					t.Errorf("rest suffix[%d] = %q, want %q", i, handlers.restRoutes[i].Suffix, want.Suffix)
				}
				if len(handlers.restRoutes[i].Query) != len(want.Query) {
					t.Errorf("rest query[%d] = %v, want %v", i, handlers.restRoutes[i].Query, want.Query)
					continue
				}
				for key, values := range want.Query {
					got := handlers.restRoutes[i].Query[key]
					if len(got) != 1 || got[0] != values[0] {
						t.Errorf("rest query[%d][%q] = %v, want %v", i, key, got, values)
					}
				}
			}
			if len(handlers.davPaths) != len(tc.wantDAV) {
				t.Fatalf("dav paths = %q, want %q", handlers.davPaths, tc.wantDAV)
			}
			for i, want := range tc.wantDAV {
				if handlers.davPaths[i] != want {
					t.Errorf("dav path[%d] = %q, want %q", i, handlers.davPaths[i], want)
				}
			}
		})
	}
}

// TestUnknownRouteResponse pins the unknown-route golden: text/plain body
// with a trailing newline, no-store, and Connection close.
func TestUnknownRouteResponse(t *testing.T) {
	handlers := &recordingHandlers{}
	router := newTestRouter(t, handlers, "/dav", "/api/v1")
	recorder := serveRoute(t, router, "GET", "/nope")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != "unknown route\n" {
		t.Errorf("body = %q, want %q", body, "unknown route\n")
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != "14" {
		t.Errorf("Content-Length = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := recorder.Header().Get("Connection"); got != "close" {
		t.Errorf("Connection = %q, want close", got)
	}
}

// TestPatchOutsideRestIsNotImplemented pins the PATCH golden (501 text with
// close, not the 404 every other method gives).
func TestPatchOutsideRestIsNotImplemented(t *testing.T) {
	handlers := &recordingHandlers{}
	router := newTestRouter(t, handlers, "/dav", "/api/v1")
	recorder := serveRoute(t, router, "PATCH", "/dav/x")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != "WPS rename/move is not available\n" {
		t.Errorf("body = %q", body)
	}
	if got := recorder.Header().Get("Connection"); got != "close" {
		t.Errorf("Connection = %q", got)
	}
}

// TestOptionsIsNotLimitedToDavPrefix pins contract DAV-OPTIONS-002: OPTIONS
// answers with fixed capabilities on any path.
func TestOptionsIsNotLimitedToDavPrefix(t *testing.T) {
	handlers := &recordingHandlers{}
	router := newTestRouter(t, handlers, "/dav", "/api/v1")
	for _, target := range []string{"/dav/", "/not-a-route", "/healthz", "/api/v1/entries", "*"} {
		recorder := serveRoute(t, router, "OPTIONS", target)
		if recorder.Code != http.StatusOK {
			t.Errorf("OPTIONS %s: status = %d", target, recorder.Code)
			continue
		}
		if got := recorder.Header().Get("DAV"); got != "1,2" {
			t.Errorf("OPTIONS %s: DAV = %q", target, got)
		}
		want := "OPTIONS, PROPFIND, GET, HEAD, PUT, MKCOL, DELETE, MOVE, COPY, LOCK, UNLOCK"
		if got := recorder.Header().Get("Allow"); got != want {
			t.Errorf("OPTIONS %s: Allow = %q", target, got)
		}
		if body := recorder.Body.String(); body != "" {
			t.Errorf("OPTIONS %s: body = %q", target, body)
		}
		if got := recorder.Header().Get("Content-Length"); got != "0" {
			t.Errorf("OPTIONS %s: Content-Length = %q", target, got)
		}
	}
}

// TestUnknownMethodReturnsStdlibPage pins the unknown-method golden: 501,
// HTML page, Connection close, no Cache-Control.
func TestUnknownMethodReturnsStdlibPage(t *testing.T) {
	handlers := &recordingHandlers{}
	router := newTestRouter(t, handlers, "/dav", "/api/v1")
	for _, method := range []string{"TRACE", "FOO", "CONNECT"} {
		recorder := serveRoute(t, router, method, "/dav/")
		if recorder.Code != http.StatusNotImplemented {
			t.Errorf("%s: status = %d", method, recorder.Code)
			continue
		}
		if got := recorder.Header().Get("Content-Type"); got != "text/html;charset=utf-8" {
			t.Errorf("%s: Content-Type = %q", method, got)
		}
		if got := recorder.Header().Get("Connection"); got != "close" {
			t.Errorf("%s: Connection = %q", method, got)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "" {
			t.Errorf("%s: unexpected Cache-Control %q", method, got)
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "Unsupported method ('"+method+"').") {
			t.Errorf("%s: body missing method name: %q", method, body)
		}
	}
}

// TestCustomPrefixes pins configurable prefixes end to end.
func TestCustomPrefixes(t *testing.T) {
	handlers := &recordingHandlers{}
	router := newTestRouter(t, handlers, "wps-dav/", "api")
	if router.DAVPrefix() != "/wps-dav" || router.RESTPrefix() != "/api" {
		t.Fatalf("normalized prefixes = %q / %q", router.DAVPrefix(), router.RESTPrefix())
	}
	recorder := serveRoute(t, router, "GET", "/wps-dav/a%20b")
	if recorder.Code != 200 || len(handlers.davPaths) != 1 || handlers.davPaths[0] != "/a b" {
		t.Errorf("custom dav route: status %d, dav %q", recorder.Code, handlers.davPaths)
	}
	recorder = serveRoute(t, router, "GET", "/api/status")
	if recorder.Code != 200 || len(handlers.restRoutes) != 1 || handlers.restRoutes[0].Suffix != "status" {
		t.Errorf("custom rest route: status %d, rest %v", recorder.Code, handlers.restRoutes)
	}
	recorder = serveRoute(t, router, "GET", "/dav/x")
	if recorder.Code != 404 {
		t.Errorf("default prefix must not match a custom-prefix router: %d", recorder.Code)
	}
}

// TestRootPrefixDegenerateMatchesOnlyExactRoot documents the Python quirk
// kept for parity: urlsplit consumes a leading "//" as an authority, so a
// "/" prefix matches only the bare root — no subpath ever reaches DAV.
func TestRootPrefixDegenerateMatchesOnlyExactRoot(t *testing.T) {
	handlers := &recordingHandlers{}
	router := newTestRouter(t, handlers, "/", "/api/v1")
	recorder := serveRoute(t, router, "PROPFIND", "/")
	if recorder.Code != 200 || len(handlers.davPaths) != 1 || handlers.davPaths[0] != "/" {
		t.Errorf("root prefix PROPFIND /: status %d, dav %q", recorder.Code, handlers.davPaths)
	}
	recorder = serveRoute(t, router, "PROPFIND", "//x")
	if recorder.Code != 404 {
		t.Errorf("urlsplit turns //x into an empty path, so it stays unknown: %d", recorder.Code)
	}
	recorder = serveRoute(t, router, "PROPFIND", "/docs")
	if recorder.Code != 404 {
		t.Errorf("root prefix never matches subpaths: %d", recorder.Code)
	}
}

// TestFragmentStrippedLikeUrlsplit keeps a request-line fragment from
// reaching the business path (Python's urlsplit drops it).
func TestFragmentStrippedLikeUrlsplit(t *testing.T) {
	handlers := &recordingHandlers{}
	router := newTestRouter(t, handlers, "/dav", "/api/v1")
	recorder := serveRoute(t, router, "GET", "/dav/x#y")
	if recorder.Code != 200 || len(handlers.davPaths) != 1 || handlers.davPaths[0] != "/x" {
		t.Errorf("fragment handling: status %d, dav %q", recorder.Code, handlers.davPaths)
	}
}

// TestAbsoluteFormTarget mirrors urlsplit on absolute-form request targets.
func TestAbsoluteFormTarget(t *testing.T) {
	handlers := &recordingHandlers{}
	router := newTestRouter(t, handlers, "/dav", "/api/v1")
	recorder := serveRoute(t, router, "GET", "http://h/dav/x")
	if recorder.Code != 200 || len(handlers.davPaths) != 1 || handlers.davPaths[0] != "/x" {
		t.Errorf("absolute-form handling: status %d, dav %q", recorder.Code, handlers.davPaths)
	}
}

// rawExchange writes a hand-built request to a live server and returns the
// raw response bytes, so transport-level behavior is observable.
func rawExchange(t *testing.T, address, request string) string {
	t.Helper()
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

// TestLiveServerRouting runs the route table through the real net/http
// transport: encoded slashes survive, unknown routes close the connection,
// and malformed escapes are rejected by the transport itself.
func TestLiveServerRouting(t *testing.T) {
	handlers := &recordingHandlers{}
	router := newTestRouter(t, handlers, "/dav", "/api/v1")
	server := httptest.NewServer(router)
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")

	response := rawExchange(t, address, "GET /dav/a%2Fb HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 200") {
		t.Fatalf("encoded slash request failed: %s", response)
	}
	if len(handlers.davPaths) != 1 || handlers.davPaths[0] != "/a/b" {
		t.Errorf("encoded slash decoded = %q", handlers.davPaths)
	}

	response = rawExchange(t, address, "GET /nope HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 404") {
		t.Fatalf("unknown route status: %s", response)
	}
	if !strings.Contains(response, "Connection: close") {
		t.Errorf("unknown route must close: %s", response)
	}
	if !strings.Contains(response, "unknown route\n") {
		t.Errorf("unknown route body missing: %s", response)
	}

	// Documented transport deviation: Go rejects malformed escapes with 400
	// before the router runs; Python passes them through as literal text.
	response = rawExchange(t, address, "GET /dav/%2G HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 400") {
		t.Errorf("malformed escape: %s", response)
	}
	if len(handlers.davPaths) != 1 {
		t.Errorf("malformed escape must not reach the router: %q", handlers.davPaths)
	}

	response = rawExchange(t, address, "FOO /dav/ HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 501") {
		t.Errorf("unknown method: %s", response)
	}
	if !strings.Contains(response, "text/html;charset=utf-8") {
		t.Errorf("unknown method content type: %s", response)
	}
}
