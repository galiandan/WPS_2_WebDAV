package httpserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Middleware wraps one handler into the next.
type Middleware func(http.Handler) http.Handler

// assemble applies the middlewares outermost first around the terminal
// handler: the first middleware in the slice is the outermost wrapper.
func assemble(middlewares []Middleware, terminal http.Handler) http.Handler {
	handler := terminal
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

// ChainConfig assembles the fixed middleware order from the migration plan:
// panic recovery, connection/request boundary, request ID, security log,
// health special case, Basic Auth, mutation origin check, and finally the
// router. Error mapping (B602) sits inside the router dispatch where the
// REST/DAV context is known.
type ChainConfig struct {
	// Router is the explicit router (B600); the terminal stage of the chain.
	Router http.Handler
	// Health answers GET /healthz before authentication and must not touch
	// storage.
	Health http.HandlerFunc
	// Auth configures adapter-side Basic authentication.
	Auth BasicAuthConfig
	// Log receives one line per request: request ID, method, and the
	// query-stripped path. It is the security log sink; query values,
	// headers, and credentials never reach it.
	Log func(requestID, method, path string)
	// PanicLog receives recovered panics with the goroutine stack. Request
	// data is never included; nil disables panic logging.
	PanicLog func(recovered any, stack []byte)
	// NewRequestID builds the correlation ID for one request. Tests inject
	// deterministic generators; nil selects the random hex default.
	NewRequestID func() string
}

// NewChain wires the middleware order. It fails at construction when the
// configuration cannot ever authenticate (credential files without a
// reader), so assembly errors never surface per request.
func NewChain(config ChainConfig) (http.Handler, error) {
	if config.Router == nil {
		return nil, errChainConfig("a router is required")
	}
	if config.Health == nil {
		return nil, errChainConfig("a health handler is required")
	}
	if config.NewRequestID == nil {
		config.NewRequestID = randomRequestID
	}
	auth, err := newBasicAuth(config.Auth)
	if err != nil {
		return nil, err
	}
	return assemble([]Middleware{
		recoverPanics(config.PanicLog),
		requestBoundary(),
		requestID(config.NewRequestID),
		securityLog(config.Log),
		healthShim(config.Health),
		auth.middleware(),
		mutationOrigin(),
	}, config.Router), nil
}

type chainConfigError string

func (e chainConfigError) Error() string { return string(e) }

func errChainConfig(message string) error { return chainConfigError(message) }

// recoverPanics converts panics into a fixed 500 without echoing the stack.
// The stack may be logged server-side; no request data is attached to it.
func recoverPanics(panicLog func(recovered any, stack []byte)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}
				if panicLog != nil {
					stack := make([]byte, 8192)
					stack = stack[:runtime.Stack(stack, false)]
					panicLog(recovered, stack)
				}
				sendError(w, r, http.StatusInternalServerError, "internal server error", false, nil, false)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestBoundary mirrors Python's AdapterRequestHandler.parse_request and
// the stdlib method dispatch: framing violations, then methods without a
// do_* implementation, are rejected before authentication exactly as in
// Python where both run before the do_* body. The router keeps its own
// method check as defense in depth when served without this chain.
func requestBoundary() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Go decodes chunked transfer coding before the handler runs;
			// its presence means the request used a framing this adapter
			// forbids, whatever Go managed to decode.
			if len(r.TransferEncoding) > 0 {
				sendError(w, r, http.StatusBadRequest, "Transfer-Encoding is not supported", false, nil, true)
				return
			}
			contentLengths := r.Header.Values("Content-Length")
			if len(contentLengths) > 1 {
				// Unreachable behind Go's transport (it rejects duplicates
				// first); kept so the boundary stays self-contained.
				sendError(w, r, http.StatusBadRequest, "multiple Content-Length headers are not supported", false, nil, true)
				return
			}
			if methodForbidsBody(r.Method) && len(contentLengths) > 0 {
				if declared, err := strconv.Atoi(strings.TrimSpace(contentLengths[0])); err != nil || declared != 0 {
					sendError(w, r, http.StatusBadRequest, "request body is not supported for this method", false, nil, true)
					return
				}
			}
			if _, known := knownMethods[r.Method]; !known {
				sendUnsupportedMethod(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// methodForbidsBody mirrors the Python check: GET, HEAD, and OPTIONS must
// not carry a body.
func methodForbidsBody(method string) bool {
	return method == "GET" || method == "HEAD" || method == "OPTIONS"
}

// requestID attaches a per-request correlation ID to the context. The ID is
// server-side only: it is never echoed in a response header, which would add
// observable behavior Python does not have.
type requestIDKey struct{}

func requestID(newID func() string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := newID()
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
		})
	}
}

// RequestIDFrom returns the correlation ID attached by the chain, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func randomRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "unavailable"
	}
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 32)
	for i, b := range buf {
		out[i*2] = hexDigits[b>>4]
		out[i*2+1] = hexDigits[b&0x0F]
	}
	return string(out)
}

// securityLog mirrors Python's log_message redaction: method and path only,
// query stripped, control characters replaced. The plan fixes its position
// after the request boundary, so boundary-rejected requests are not logged.
func securityLog(log func(requestID, method, path string)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if log != nil {
				path, _ := SplitRequestTarget(r.RequestURI)
				log(RequestIDFrom(r.Context()), r.Method, sanitizeLogPath(path))
			}
			next.ServeHTTP(w, r)
		})
	}
}

func sanitizeLogPath(path string) string {
	var out strings.Builder
	for _, r := range path {
		if r >= 0x20 && r != 0x7F {
			out.WriteRune(r)
		} else {
			out.WriteByte('?')
		}
	}
	return out.String()
}

// healthShim answers GET /healthz before authentication. Other methods on
// /healthz fall through: they stay auth-exempt (the auth middleware checks
// the same path) but route normally, mirroring Python's _authorise and
// do_GET ordering.
func healthShim(health http.HandlerFunc) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path, _ := SplitRequestTarget(r.RequestURI)
			if r.Method == "GET" && IsHealthPath(path) {
				health(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BasicAuthConfig mirrors Python's BasicAuth: inline credentials or file
// paths, hot-read per request, constant-time comparison. WPS cookies are
// never used as adapter credentials.
type BasicAuthConfig struct {
	Username     string
	Password     string
	UsernameFile string
	PasswordFile string
	// ReadSecret loads a credential file on every request. Read errors
	// become empty credentials, exactly like Python catching WpsApiError.
	// httpserver must not import securefile (dependency rules), so the app
	// assembly injects securefile.ReadSecret here.
	ReadSecret func(path string) (string, error)
}

type basicAuth struct {
	config BasicAuthConfig
}

func newBasicAuth(config BasicAuthConfig) (basicAuth, error) {
	if (config.UsernameFile != "" || config.PasswordFile != "") && config.ReadSecret == nil {
		return basicAuth{}, errChainConfig("a secret reader is required for credential files")
	}
	return basicAuth{config: config}, nil
}

// enabled mirrors the Python property: any configured credential source
// turns authentication on, even a half-configured one (D-05), which then
// rejects everything.
func (a basicAuth) enabled() bool {
	return a.config.Username != "" || a.config.Password != "" ||
		a.config.UsernameFile != "" || a.config.PasswordFile != ""
}

// values hot-reads the credential files on every call.
func (a basicAuth) values() (string, string) {
	username := a.config.Username
	if a.config.UsernameFile != "" {
		username = a.readCredential(a.config.UsernameFile)
	}
	password := a.config.Password
	if a.config.PasswordFile != "" {
		password = a.readCredential(a.config.PasswordFile)
	}
	return username, password
}

func (a basicAuth) readCredential(path string) string {
	value, err := a.config.ReadSecret(path)
	if err != nil {
		return ""
	}
	return value
}

// accepts mirrors Python's BasicAuth.accepts including the strict base64
// validation and the constant-time comparisons.
func (a basicAuth) accepts(header string) bool {
	username, password := a.values()
	if username == "" || password == "" || header == "" {
		return false
	}
	scheme, encoded, hasSeparator := strings.Cut(header, " ")
	if !hasSeparator || !strings.EqualFold(scheme, "basic") {
		return false
	}
	encoded = strings.TrimSpace(encoded)
	if !validBase64Alphabet(encoded) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !utf8.Valid(decoded) {
		return false
	}
	suppliedUser, suppliedPassword, hasColon := strings.Cut(string(decoded), ":")
	if !hasColon {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(suppliedUser), []byte(username)) == 1
	passwordOK := subtle.ConstantTimeCompare([]byte(suppliedPassword), []byte(password)) == 1
	return userOK && passwordOK
}

const base64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="

// validBase64Alphabet mirrors b64decode(validate=True): any byte outside the
// standard alphabet (before decoding) rejects the header outright.
func validBase64Alphabet(encoded string) bool {
	for i := 0; i < len(encoded); i++ {
		if strings.IndexByte(base64Alphabet, encoded[i]) < 0 {
			return false
		}
	}
	return true
}

func (a basicAuth) middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path, _ := SplitRequestTarget(r.RequestURI)
			// Python's _authorise exempts the health path for every method;
			// non-GET health requests still route (and 404) later.
			if IsHealthPath(path) || !a.enabled() {
				next.ServeHTTP(w, r)
				return
			}
			if a.accepts(r.Header.Get("Authorization")) {
				next.ServeHTTP(w, r)
				return
			}
			sendUnauthorized(w)
		})
	}
}

// sendUnauthorized mirrors Python's _authorise rejection byte for byte:
// challenge, close, and an empty body with no Content-Type and no
// Cache-Control header.
func sendUnauthorized(w http.ResponseWriter) {
	header := w.Header()
	header.Set("WWW-Authenticate", `Basic realm="wps-adapter"`)
	header.Set("Connection", "close")
	header.Set("Content-Length", "0")
	w.WriteHeader(http.StatusUnauthorized)
}

// mutationMethods are the methods Python guards with _allow_mutation_origin:
// all writes plus the state-changing DAV methods.
var mutationMethods = map[string]struct{}{
	"PUT": {}, "POST": {}, "DELETE": {}, "PATCH": {},
	"MKCOL": {}, "MOVE": {}, "COPY": {}, "LOCK": {}, "UNLOCK": {},
}

// mutationOrigin rejects cross-origin mutations. Origin wins; only when it
// is absent does Referer decide; with neither header the request passes.
// The failure response is always text (the check runs before routing).
func mutationOrigin() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, guarded := mutationMethods[r.Method]; guarded {
				if !allowMutationOrigin(r) {
					sendError(w, r, http.StatusForbidden, "cross-origin mutation is not allowed", false,
						map[string]string{"Connection": "close"}, true)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func allowMutationOrigin(r *http.Request) bool {
	if originValues := r.Header.Values("Origin"); len(originValues) > 0 {
		return originMatchesRequestHost(r, originValues[0], false)
	}
	refererValues := r.Header.Values("Referer")
	if len(refererValues) == 0 {
		return true
	}
	return originMatchesRequestHost(r, refererValues[0], true)
}

// originMatchesRequestHost mirrors Python's _origin_matches_request_host:
// strict URL shape, hostname equality ignoring a trailing dot and case, and
// a port rule that lets reverse proxies omit either port.
func originMatchesRequestHost(r *http.Request, value string, referer bool) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if b := value[i]; b < 0x20 || b == 0x7F {
			return false
		}
	}
	parts, err := url.Parse(value)
	if err != nil {
		return false
	}
	if parts.Scheme != "http" && parts.Scheme != "https" {
		return false
	}
	hostname := parts.Hostname()
	if hostname == "" || parts.User != nil || parts.RawQuery != "" || parts.Fragment != "" {
		return false
	}
	if !referer && parts.EscapedPath() != "" && parts.EscapedPath() != "/" {
		return false
	}
	requestHost := urlParseAuthority(r.Host)
	if requestHost.hostname == "" {
		return false
	}
	if trimHostCase(hostname) != trimHostCase(requestHost.hostname) {
		return false
	}
	return parts.Port() == "" || requestHost.port == "" || parts.Port() == requestHost.port
}

type authorityParts struct {
	hostname string
	port     string
}

// urlParseAuthority mirrors Python's urlsplit("//" + request_host) on the
// Host header value.
func urlParseAuthority(host string) authorityParts {
	parts, err := url.Parse("//" + host)
	if err != nil {
		return authorityParts{}
	}
	return authorityParts{hostname: parts.Hostname(), port: parts.Port()}
}

func trimHostCase(hostname string) string {
	return strings.ToLower(strings.TrimRight(hostname, "."))
}
