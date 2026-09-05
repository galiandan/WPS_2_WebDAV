// The signed object client speaks to WPS object-storage hosts using
// pre-signed URLs. It is deliberately a separate transport from the
// control-plane client: it holds no credentials, no cookie jar, and its
// request builder refuses credential headers, so signature-bearing hosts can
// never observe the browser session.

package wps

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// SignedTarget is a validated signed URL split into its dialing parts. The
// target keeps the raw percent-encoded path and query so the wire request
// matches what WPS signed.
type SignedTarget struct {
	Host   string
	Port   int // 0 means the default HTTPS port 443.
	Target string
}

// SignedHeader is one explicitly allowed header for a signed object request.
// Signed requests carry no other headers than these and the Host.
type SignedHeader struct {
	Name  string
	Value string
}

// SignedObjectClient sends requests to signed object-storage URLs.
type SignedObjectClient struct {
	config    Config
	suffix    string // normalized object-storage host suffix
	transport http.RoundTripper
}

// NewSignedObjectClient builds the signed transport: TLS is verified, no
// proxy environment applies (Python's raw HTTPS connections dial directly),
// and the configured timeout bounds connection phases. A http.RoundTripper
// never follows redirects, matching the raw connection behavior.
func NewSignedObjectClient(config Config) *SignedObjectClient {
	return &SignedObjectClient{
		config:    config,
		suffix:    normalizeObjectSuffix(config.ObjectStorageHostSuffix),
		transport: newSignedTransport(config.Timeout),
	}
}

func newSignedTransport(timeout float64) http.RoundTripper {
	duration := seconds(timeout)
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: duration, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   duration,
		ResponseHeaderTimeout: duration,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// ParseSignedTarget mirrors _signed_target. Only HTTPS URLs on object hosts
// inside the configured suffix, without credentials, fragments, or ports
// other than the default, may receive object requests, and the URL must not
// carry control characters. Every rejection is a WpsAPIError naming the
// operation without echoing the URL.
func ParseSignedTarget(signedURL string, operation string, objectSuffix string) (SignedTarget, error) {
	if hasControlChars(signedURL) {
		return SignedTarget{}, model.NewWpsAPIError(operation, 0, model.WpsCategoryUpstream)
	}
	parsed, err := url.Parse(signedURL)
	if err != nil {
		return SignedTarget{}, model.NewWpsAPIError(operation, 0, model.WpsCategoryUpstream)
	}
	username, password := "", ""
	if parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
	}
	host := strings.ToLower(parsed.Hostname())
	suffix := normalizeObjectSuffix(objectSuffix)
	trimmed := strings.ToLower(strings.TrimRight(host, "."))
	hostAllowed := suffix != "" && host != "" &&
		(trimmed == suffix || strings.HasSuffix(trimmed, "."+suffix))
	port := -1
	if rawPort := parsed.Port(); rawPort != "" {
		value, convErr := strconv.Atoi(rawPort)
		if convErr != nil {
			return SignedTarget{}, model.NewWpsAPIError(operation, 0, model.WpsCategoryUpstream)
		}
		port = value
	}
	if parsed.Scheme != "https" ||
		!hostAllowed ||
		username != "" || password != "" ||
		parsed.Fragment != "" ||
		(port != -1 && port != 443) {
		return SignedTarget{}, model.NewWpsAPIError(operation, 0, model.WpsCategoryUpstream)
	}
	target := parsed.EscapedPath()
	if target == "" {
		target = "/"
	}
	if parsed.RawQuery != "" {
		target += "?" + parsed.RawQuery
	}
	if port < 0 {
		port = 0
	}
	return SignedTarget{Host: host, Port: port, Target: target}, nil
}

// Do sends one signed object request. It accepts no credentials: only the
// explicitly passed headers are sent, and credential headers are rejected
// outright. The response body is the caller's responsibility to close.
// Transport failures surface as a WpsAPIError in the unavailable category
// without echoing the signed URL, whose query carries signature credentials.
func (c *SignedObjectClient) Do(
	operation string,
	method string,
	signedURL string,
	headers []SignedHeader,
	body io.Reader,
	contentLength int64,
) (*http.Response, error) {
	if _, err := ParseSignedTarget(signedURL, operation, c.suffix); err != nil {
		return nil, err
	}
	for _, header := range headers {
		name := strings.ToLower(header.Name)
		if name == "cookie" || name == "authorization" || strings.Contains(name, "csrf") {
			return nil, model.NewWpsAPIError(operation, 0, model.WpsCategoryUpstream)
		}
	}
	request, err := http.NewRequest(method, signedURL, body)
	if err != nil {
		return nil, model.NewWpsAPIError(operation, 0, model.WpsCategoryUpstream)
	}
	for _, header := range headers {
		request.Header.Set(header.Name, header.Value)
	}
	if body != nil {
		request.ContentLength = contentLength
	}
	if request.URL.Port() == "443" {
		request.Host = request.URL.Hostname()
	}
	response, err := c.transport.RoundTrip(request)
	if err != nil {
		return nil, model.NewWpsAPIError(operation, 0, model.WpsCategoryUnavailable)
	}
	return response, nil
}
