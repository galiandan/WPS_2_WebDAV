// Package remotefetch downloads one public HTTP(S) object through a DNS-pinned
// transport. Source URLs may contain bearer queries and are never returned in
// results, retained by the client, or included in errors.
package remotefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/opdeadline"
)

const (
	MaxURLBytes     = 8 << 10
	MaxRedirects    = 5
	maxDNSAnswers   = 32
	readBufferBytes = 64 << 10
)

var (
	ErrInvalidURL       = errors.New("remote URL is invalid")
	ErrForbiddenAddress = errors.New("remote address is not a public Internet address")
	ErrDNS              = errors.New("remote hostname could not be resolved")
	ErrNetwork          = errors.New("remote connection failed")
	ErrTimeout          = errors.New("remote connection timed out")
	ErrStatus           = errors.New("remote server did not return a complete file")
	ErrEncoding         = errors.New("remote response uses an unsupported content encoding")
	ErrRedirects        = errors.New("remote server exceeded the redirect limit")
	ErrTooLarge         = errors.New("remote file exceeds the size limit")
	ErrIncomplete       = errors.New("remote response ended before the declared file length")
	ErrRead             = errors.New("remote response could not be read")
	ErrWrite            = errors.New("download destination could not be written")
	ErrReservation      = errors.New("download destination could not reserve space")
	ErrInvalidLimit     = errors.New("a positive download size limit and destination are required")
)

type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("remote server returned HTTP %d", e.Status) }
func (e *StatusError) Unwrap() error { return ErrStatus }

type Result struct {
	Bytes       int64
	ContentType string
}

// ExpectedSizer is an optional destination hook. Known lengths are reported
// before any body write; -1 denotes an unknown length. A rejection closes the
// response without starting the copy, so disk budgets can reserve atomically.
type ExpectedSizer interface{ SetExpectedSize(int64) error }

// Config supplies controlled transport seams for offline tests. Callers must
// not accept these callbacks or timeout settings from HTTP request input.
type Config struct {
	LookupIP            func(context.Context, string) ([]net.IPAddr, error)
	DialContext         func(context.Context, string, string) (net.Conn, error)
	TotalTimeout        time.Duration
	IdleTimeout         time.Duration
	HeaderTimeout       time.Duration
	TLSHandshakeTimeout time.Duration
	DNSTimeout          time.Duration
	DialTimeout         time.Duration
}
type Client struct{ config Config }

func NewClient(config Config) *Client {
	if config.LookupIP == nil {
		config.LookupIP = net.DefaultResolver.LookupIPAddr
	}
	if config.DialContext == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		config.DialContext = dialer.DialContext
	}
	if config.TotalTimeout <= 0 {
		config.TotalTimeout = 30 * time.Minute
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = time.Minute
	}
	if config.HeaderTimeout <= 0 {
		config.HeaderTimeout = 20 * time.Second
	}
	if config.TLSHandshakeTimeout <= 0 {
		config.TLSHandshakeTimeout = 10 * time.Second
	}
	if config.DNSTimeout <= 0 {
		config.DNSTimeout = 10 * time.Second
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = 10 * time.Second
	}
	return &Client{config: config}
}

func Fetch(ctx context.Context, sourceURL string, dst io.Writer, maxBytes int64, progress func(int64)) (Result, error) {
	return NewClient(Config{}).Fetch(ctx, sourceURL, dst, maxBytes, progress)
}

// ValidateURL applies queue-time syntax and literal-address checks without DNS
// or network access. Fetch still resolves and validates every hostname at use.
func ValidateURL(sourceURL string) error {
	parsed, err := parseURL(sourceURL)
	if err != nil {
		return err
	}
	if address, err := netip.ParseAddr(parsed.Hostname()); err == nil && !publicAddress(address) {
		return ErrForbiddenAddress
	}
	return nil
}

type pinContextKey struct{}
type pinnedTarget struct {
	host, port     string
	addresses      []netip.Addr
	requestContext context.Context
}

func (c *Client) Fetch(ctx context.Context, sourceURL string, dst io.Writer, maxBytes int64, progress func(int64)) (Result, error) {
	result := Result{}
	if dst == nil || maxBytes <= 0 {
		return result, ErrInvalidLimit
	}
	ctx, cancel := context.WithTimeout(ctx, c.config.TotalTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	parsed, err := parseURL(sourceURL)
	if err != nil {
		return result, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return result, ErrInvalidURL
	}
	if err := c.prepare(request); err != nil {
		return result, redact(err, ctx, ErrNetwork)
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: c.dialPinned, DisableKeepAlives: true, DisableCompression: true,
		MaxConnsPerHost: 1, MaxResponseHeaderBytes: 64 << 10,
		ResponseHeaderTimeout: c.config.HeaderTimeout, TLSHandshakeTimeout: c.config.TLSHandshakeTimeout,
		ExpectContinueTimeout: time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) > MaxRedirects {
			return ErrRedirects
		}
		return c.prepare(next)
	}}
	response, err := client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return result, redact(err, ctx, ErrNetwork)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, &StatusError{Status: response.StatusCode}
	}
	encodings := response.Header.Values("Content-Encoding")
	if len(encodings) > 1 || (len(encodings) == 1 && strings.TrimSpace(encodings[0]) != "" && !strings.EqualFold(strings.TrimSpace(encodings[0]), "identity")) {
		return result, ErrEncoding
	}
	if response.ContentLength > maxBytes {
		return result, ErrTooLarge
	}
	if sizer, ok := dst.(ExpectedSizer); ok {
		if err := sizer.SetExpectedSize(response.ContentLength); err != nil {
			return result, ErrReservation
		}
	}
	result.ContentType = safeContentType(response.Header.Get("Content-Type"))
	if progress != nil {
		progress(0)
	}
	buffer := make([]byte, readBufferBytes)
	for {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		remaining := maxBytes - result.Bytes
		size := len(buffer)
		if remaining < int64(size) {
			size = int(remaining) + 1
		}
		n, readErr := response.Body.Read(buffer[:size])
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		writeSize := n
		overflow := int64(n) > remaining
		if overflow {
			writeSize = int(remaining)
		}
		if writeSize > 0 {
			written, writeErr := dst.Write(buffer[:writeSize])
			if written < 0 || written > writeSize {
				return result, ErrWrite
			}
			result.Bytes += int64(written)
			if progress != nil {
				progress(result.Bytes)
			}
			if writeErr != nil || written != writeSize {
				return result, ErrWrite
			}
		}
		if overflow {
			return result, ErrTooLarge
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if response.ContentLength >= 0 && result.Bytes != response.ContentLength {
					return result, ErrIncomplete
				}
				return result, nil
			}
			if errors.Is(readErr, io.ErrUnexpectedEOF) {
				return result, ErrIncomplete
			}
			return result, redact(readErr, ctx, ErrRead)
		}
		if n == 0 {
			return result, ErrRead
		}
	}
}

func parseURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > MaxURLBytes || !utf8.ValidString(raw) {
		return nil, ErrInvalidURL
	}
	for _, r := range raw {
		if r < 32 || r == 127 {
			return nil, ErrInvalidURL
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, ErrInvalidURL
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || strings.ContainsAny(parsed.Hostname(), "%\\") {
		return nil, ErrInvalidURL
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, ErrInvalidURL
		}
	}
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed, nil
}

func (c *Client) prepare(request *http.Request) error {
	parsed, err := parseURL(request.URL.String())
	if err != nil {
		return err
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port == "" {
		port = "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
	}
	addresses, err := c.resolve(request.Context(), host)
	if err != nil {
		return err
	}
	pin := pinnedTarget{host: host, port: port, addresses: addresses, requestContext: request.Context()}
	*request = *request.WithContext(context.WithValue(request.Context(), pinContextKey{}, pin))
	request.URL = parsed
	// Go's redirect helper normally adds Referer including the previous query.
	// Reset headers so signed URLs, cookies and authorization never propagate.
	request.Header = make(http.Header)
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", "wps-adapter-fetch")
	return nil
}

func (c *Client) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		address = address.Unmap()
		if !publicAddress(address) {
			return nil, ErrForbiddenAddress
		}
		return []netip.Addr{address}, nil
	}
	lookupContext, cancel := context.WithTimeout(ctx, c.config.DNSTimeout)
	defer cancel()
	answers, err := c.config.LookupIP(lookupContext, host)
	if err != nil {
		return nil, redact(err, lookupContext, ErrDNS)
	}
	if len(answers) == 0 || len(answers) > maxDNSAnswers {
		return nil, ErrDNS
	}
	addresses := make([]netip.Addr, 0, len(answers))
	seen := make(map[netip.Addr]bool)
	for _, answer := range answers {
		address, ok := netip.AddrFromSlice(answer.IP)
		if !ok || answer.Zone != "" {
			return nil, ErrForbiddenAddress
		}
		address = address.Unmap()
		if !publicAddress(address) {
			return nil, ErrForbiddenAddress
		}
		if !seen[address] {
			addresses = append(addresses, address)
			seen[address] = true
		}
	}
	return addresses, nil
}

func (c *Client) dialPinned(ctx context.Context, network, address string) (net.Conn, error) {
	pin, ok := ctx.Value(pinContextKey{}).(pinnedTarget)
	if !ok {
		return nil, ErrForbiddenAddress
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || !strings.EqualFold(host, pin.host) || port != pin.port {
		return nil, ErrForbiddenAddress
	}
	dialContext, cancel := context.WithTimeout(ctx, c.config.DialTimeout)
	defer cancel()
	stop := context.AfterFunc(pin.requestContext, cancel)
	defer stop()
	if pin.requestContext.Err() != nil {
		return nil, pin.requestContext.Err()
	}
	for _, ip := range pin.addresses {
		if !publicAddress(ip) {
			return nil, ErrForbiddenAddress
		}
		connection, err := c.config.DialContext(dialContext, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return opdeadline.Wrap(connection, c.config.IdleTimeout), nil
		}
		if dialContext.Err() != nil {
			return nil, dialContext.Err()
		}
	}
	return nil, ErrNetwork
}

func safeContentType(value string) string {
	if len(value) > 256 {
		return "application/octet-stream"
	}
	kind, _, err := mime.ParseMediaType(value)
	if err != nil || !strings.Contains(kind, "/") {
		return "application/octet-stream"
	}
	return kind
}
func redact(err error, ctx context.Context, fallback error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, known := range []error{ErrInvalidURL, ErrForbiddenAddress, ErrDNS, ErrNetwork, ErrTimeout, ErrRedirects, ErrTooLarge, ErrIncomplete, ErrRead, ErrWrite} {
		if errors.Is(err, known) {
			return known
		}
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return ErrTimeout
	}
	return fallback
}

var blockedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.31.196.0/24"), netip.MustParsePrefix("192.52.193.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("168.63.129.16/32"),
}
var globalIPv6 = netip.MustParsePrefix("2000::/3")
var blockedIPv6 = []netip.Prefix{netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("2620:4f:8000::/48"), netip.MustParsePrefix("3ffe::/16"), netip.MustParsePrefix("3fff::/20")}

func publicAddress(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	if address.Is4() {
		for _, prefix := range blockedIPv4 {
			if prefix.Contains(address) {
				return false
			}
		}
		return true
	}
	if !globalIPv6.Contains(address) {
		return false
	}
	for _, prefix := range blockedIPv6 {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
