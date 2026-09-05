// Package httpserver implements the adapter's HTTP facade: the explicit
// router, the middleware chain, response primitives, and the REST/WebDAV
// dispatchers.
//
// Routing contract (decision D-04): the wire request path is decoded exactly
// once. The router matches route prefixes on the raw request target — the
// mirror of Python's urlsplit(self.path).path — never on the already-decoded
// r.URL.Path, and never through http.ServeMux, whose automatic path cleaning
// and redirects would rewrite business paths. The extracted WebDAV business
// path is percent-decoded once here and handed to the DAV handler; storage's
// SplitRemotePath never decodes. REST queries are parsed once with Python's
// parse_qs semantics (see target.go).
package httpserver

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// HealthPath is the fixed health endpoint. Unlike the DAV and REST prefixes
// it is never configurable (Python hardcodes /healthz).
const HealthPath = "/healthz"

const (
	davCapabilityHeader   = "1,2"
	allowMethodsHeader    = "OPTIONS, PROPFIND, GET, HEAD, PUT, MKCOL, DELETE, MOVE, " + "COPY, LOCK, UNLOCK"
	unknownRouteMessage   = "unknown route"
	patchRouteMessage     = "WPS rename/move is not available"
	unsupportedMethodNote = "Unsupported method"
)

// knownMethods mirrors the do_* methods BaseHTTPRequestHandler dispatches to;
// anything else receives the stdlib-style 501 page before any routing.
var knownMethods = map[string]struct{}{
	"OPTIONS":  {},
	"GET":      {},
	"HEAD":     {},
	"PUT":      {},
	"POST":     {},
	"DELETE":   {},
	"PATCH":    {},
	"PROPFIND": {},
	"MKCOL":    {},
	"MOVE":     {},
	"COPY":     {},
	"LOCK":     {},
	"UNLOCK":   {},
}

// webAppPaths are the three fixed web page entries, matched on the raw
// request-target path.
var webAppPaths = map[string]struct{}{
	"/":     {},
	"/web":  {},
	"/web/": {},
}

// RESTRoute is one dispatch under the REST prefix. Suffix is the raw,
// still percent-encoded remainder trimmed of leading and trailing slashes —
// route names are compared literally, so encoded spellings never match. The
// business path itself travels in Query (usually the "path" parameter),
// decoded exactly once by parseQueryValues.
type RESTRoute struct {
	Suffix string
	Query  url.Values
}

// Handlers carries the route targets the application assembly wires in.
// The health, web, REST, and DAV handlers are implemented by their own
// migration stages; the router only selects between them.
type Handlers struct {
	Health   http.HandlerFunc
	WebApp   http.HandlerFunc
	WebAsset func(w http.ResponseWriter, r *http.Request, name string)
	REST     func(w http.ResponseWriter, r *http.Request, route RESTRoute)
	DAV      func(w http.ResponseWriter, r *http.Request, davPath string)
}

// RouterConfig configures the explicit router. Empty prefixes fall back to
// "/" exactly like AdapterApplication._normalise_prefix.
type RouterConfig struct {
	DAVPrefix  string
	RESTPrefix string
	Handlers   Handlers
}

// Router dispatches requests to the registered handlers without ever
// cleaning or redirecting the request path.
type Router struct {
	davPrefix  string
	restPrefix string
	handlers   Handlers
}

// NewRouter validates the handler wiring and normalises the prefixes.
func NewRouter(config RouterConfig) (*Router, error) {
	handlers := config.Handlers
	switch {
	case handlers.Health == nil:
		return nil, errors.New("a health handler is required")
	case handlers.WebApp == nil:
		return nil, errors.New("a web app handler is required")
	case handlers.WebAsset == nil:
		return nil, errors.New("a web asset handler is required")
	case handlers.REST == nil:
		return nil, errors.New("a REST handler is required")
	case handlers.DAV == nil:
		return nil, errors.New("a DAV handler is required")
	}
	return &Router{
		davPrefix:  normalizePrefix(config.DAVPrefix),
		restPrefix: normalizePrefix(config.RESTPrefix),
		handlers:   handlers,
	}, nil
}

// normalizePrefix mirrors AdapterApplication._normalise_prefix: a leading
// slash is added when missing and trailing slashes are collapsed; the empty
// value becomes the root prefix "/".
func normalizePrefix(value string) string {
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	if trimmed := strings.TrimRight(value, "/"); trimmed != "" {
		return trimmed
	}
	return "/"
}

// DAVPrefix returns the normalised WebDAV prefix for href building and
// Destination validation.
func (rt *Router) DAVPrefix() string {
	return rt.davPrefix
}

// RESTPrefix returns the normalised REST prefix.
func (rt *Router) RESTPrefix() string {
	return rt.restPrefix
}

// IsHealthPath reports whether a split request-target path is the health
// endpoint. The middleware chain uses it to serve health before
// authentication (B601), mirroring Python's _is_health short-circuit.
func IsHealthPath(path string) bool {
	return path == HealthPath
}

// ServeHTTP routes one request. The method table and the per-method route
// order mirror Python's do_* handlers exactly.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := knownMethods[r.Method]; !ok {
		sendUnsupportedMethod(w, r)
		return
	}
	path, rawQuery := SplitRequestTarget(r.RequestURI)
	if r.Method == "OPTIONS" {
		// Python answers OPTIONS on every path — inside or outside the DAV
		// prefix (contract DAV-OPTIONS-001/002) — with fixed capability
		// headers and an empty body.
		writeResponse(w, r, http.StatusOK, nil, contentTypeText, map[string]string{
			"DAV":   davCapabilityHeader,
			"Allow": allowMethodsHeader,
		}, false)
		return
	}
	switch r.Method {
	case "GET":
		if IsHealthPath(path) {
			rt.handlers.Health(w, r)
			return
		}
		if _, ok := webAppPaths[path]; ok {
			rt.handlers.WebApp(w, r)
			return
		}
		if name, ok := webAssetName(path); ok {
			rt.handlers.WebAsset(w, r, name)
			return
		}
		if route, ok := rt.restRoute(path, rawQuery); ok {
			rt.handlers.REST(w, r, route)
			return
		}
		if davPath, ok := rt.davPath(path); ok {
			rt.handlers.DAV(w, r, davPath)
			return
		}
		sendUnknownRoute(w, r)
	case "HEAD":
		// HEAD serves assets and DAV metadata but never the web page and
		// never REST (mirrors do_HEAD).
		if name, ok := webAssetName(path); ok {
			rt.handlers.WebAsset(w, r, name)
			return
		}
		if davPath, ok := rt.davPath(path); ok {
			rt.handlers.DAV(w, r, davPath)
			return
		}
		sendUnknownRoute(w, r)
	case "PUT", "DELETE":
		if route, ok := rt.restRoute(path, rawQuery); ok {
			rt.handlers.REST(w, r, route)
			return
		}
		if davPath, ok := rt.davPath(path); ok {
			rt.handlers.DAV(w, r, davPath)
			return
		}
		sendUnknownRoute(w, r)
	case "POST":
		// POST exists only for REST routes; DAV paths are unknown to it.
		if route, ok := rt.restRoute(path, rawQuery); ok {
			rt.handlers.REST(w, r, route)
			return
		}
		sendUnknownRoute(w, r)
	case "PATCH":
		// PATCH exists only for REST routes; anywhere else Python reports
		// 501 instead of 404 because rename/move via DAV is unavailable.
		if route, ok := rt.restRoute(path, rawQuery); ok {
			rt.handlers.REST(w, r, route)
			return
		}
		sendError(w, r, http.StatusNotImplemented, patchRouteMessage, false, nil, true)
	case "PROPFIND", "MKCOL", "MOVE", "COPY", "LOCK", "UNLOCK":
		if davPath, ok := rt.davPath(path); ok {
			rt.handlers.DAV(w, r, davPath)
			return
		}
		sendUnknownRoute(w, r)
	}
}

// sendUnknownRoute mirrors the do_* fallback: a text 404 and a closed
// connection (Python sets close_connection before responding).
func sendUnknownRoute(w http.ResponseWriter, r *http.Request) {
	sendError(w, r, http.StatusNotFound, unknownRouteMessage, false, nil, true)
}

// sendUnsupportedMethod mirrors BaseHTTPRequestHandler's answer for methods
// without a do_* implementation: a small stdlib-shaped HTML error page with
// Connection: close and no Cache-Control header. Go always writes the
// standard reason phrase, so the status line differs from Python's
// message-bearing one; no test pins those bytes.
func sendUnsupportedMethod(w http.ResponseWriter, r *http.Request) {
	message := unsupportedMethodNote + " ('" + r.Method + "')."
	page := `<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 4.01//EN" "http://www.w3.org/TR/html4/strict.dtd">
<html>
    <head>
        <meta http-equiv="Content-Type" content="text/html;charset=utf-8">
        <title>Error response</title>
    </head>
    <body>
        <h1>Error response</h1>
        <p>Error code: 501</p>
        <p>Message: ` + message + `</p>
        <p>Error code explanation: 501 - ` + message + `</p>
    </body>
</html>
`
	header := w.Header()
	header.Set("Content-Type", contentTypeHTML)
	header.Set("Content-Length", strconv.Itoa(len(page)))
	header.Set("Connection", "close")
	w.WriteHeader(http.StatusNotImplemented)
	if r.Method != "HEAD" {
		w.Write([]byte(page))
	}
}

// webAssetName mirrors Python: the raw remainder below "/assets/", trimmed
// of slashes. The name is deliberately not decoded — asset names are fixed
// ASCII literals, so an encoded spelling must miss like it does in Python.
func webAssetName(path string) (string, bool) {
	if !strings.HasPrefix(path, assetRoot) {
		return "", false
	}
	return strings.Trim(path[len(assetRoot):], "/"), true
}

// restRoute reports whether the raw path sits under the REST prefix and
// extracts the trimmed suffix plus the once-decoded query.
func (rt *Router) restRoute(path, rawQuery string) (RESTRoute, bool) {
	var raw string
	switch {
	case path == rt.restPrefix:
	case strings.HasPrefix(path, rt.restPrefix+"/"):
		raw = path[len(rt.restPrefix):]
	default:
		return RESTRoute{}, false
	}
	return RESTRoute{Suffix: strings.Trim(raw, "/"), Query: parseQueryValues(rawQuery)}, true
}

// davPath extracts the WebDAV business path from the raw request-target path
// and percent-decodes it exactly once (D-04). Malformed escapes stay literal
// like Python's urllib unquote; invalid UTF-8 survives in the bytes and is
// rejected by storage with Python's exact message.
func (rt *Router) davPath(path string) (string, bool) {
	if path == rt.davPrefix {
		return "/", true
	}
	if !strings.HasPrefix(path, rt.davPrefix+"/") {
		return "", false
	}
	remainder := path[len(rt.davPrefix):]
	if remainder == "" {
		return "/", true
	}
	return unquotePercent(remainder), true
}
