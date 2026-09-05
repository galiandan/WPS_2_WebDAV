// Package wps speaks the WPS control-plane protocol observed in browser
// captures and the signed object-storage protocol used for uploads and
// downloads.
//
// Two transports exist here and never mix. The control-plane client attaches
// the current Cookie and optional Origin/Referer to kdocs.cn API calls. The
// signed object client is constructed without any credential surface: it
// holds no cookie jar and its request builder rejects credential headers, so
// a signed object host can never observe WPS or Basic Auth values even when
// the owning Client holds them.
package wps

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

// Response and payload limits mirror client.py. Each protocol phase reads a
// bounded amount of upstream data: WPS JSON control responses, object
// control bodies, and multipart merge XML each get their own ceiling.
const (
	MaxJSONResponseBytes   = 8 << 20
	MaxObjectResponseBytes = 1 << 20
	MaxXMLResponseBytes    = 4 << 20
	DefaultMaxUploadBytes  = 1024 << 20
	MaxMultipartPartBuffer = 64 << 20

	MaxRemoteNameBytes = 4096
	MaxRemoteEtagBytes = 4096

	DefaultBaseURL                 = "https://365.kdocs.cn"
	DefaultObjectStorageHostSuffix = ".ag.kdocs.cn"
)

const defaultTimeoutSeconds = 30.0

// Config carries the resolved client settings; cookie values never appear
// here because credentials live behind CredentialSource.
type Config struct {
	GroupID          string
	Workspace        *workspace.WorkspaceState
	CredentialSource credentials.Source

	BaseURL        string
	AccountBaseURL string
	AutoRefresh    bool
	Referer        string
	Origin         string
	CID            string

	Timeout              float64
	StatusProbeTTL       float64
	StatusFailureBackoff float64

	UploadSpoolMemory       int64
	StreamChunkSize         int64
	MultipartThreshold      int64
	MultipartPartSize       int64
	EnableRange             bool
	UploadSpoolDir          string
	UploadResumeDir         string
	UploadMinFreeBytes      int64
	MaxUploadBytes          int64
	UploadRetries           int
	UploadRetryDelay        float64
	ObjectStorageHostSuffix string
	MaxJSONResponseBytes    int64
}

// DefaultConfig mirrors the WpsClientConfig dataclass defaults. Construct
// client configurations through this function so boolean settings start at
// their Python defaults (AutoRefresh and EnableRange are true).
func DefaultConfig(groupID string) Config {
	return Config{
		GroupID:                 groupID,
		BaseURL:                 DefaultBaseURL,
		AutoRefresh:             true,
		EnableRange:             true,
		Timeout:                 defaultTimeoutSeconds,
		StatusProbeTTL:          defaultTimeoutSeconds,
		StatusFailureBackoff:    5,
		UploadSpoolMemory:       8 << 20,
		StreamChunkSize:         1 << 20,
		MultipartThreshold:      50 << 20,
		MultipartPartSize:       10 << 20,
		UploadMinFreeBytes:      512 << 20,
		MaxUploadBytes:          DefaultMaxUploadBytes,
		UploadRetries:           2,
		UploadRetryDelay:        0.5,
		ObjectStorageHostSuffix: DefaultObjectStorageHostSuffix,
		MaxJSONResponseBytes:    MaxJSONResponseBytes,
	}
}

// Opener sends one prepared control-plane request. *http.Client satisfies
// it; tests inject fakes that record requests instead of dialing.
type Opener interface {
	Do(request *http.Request) (*http.Response, error)
}

// Client is the WPS control-plane and signed object client. It owns both
// transports and guarantees they never share state.
type Client struct {
	config Config
	opener Opener
	signed *SignedObjectClient

	// credentialRefreshLock serializes 401 refresh grants so a rotated rtk
	// cookie cannot be overwritten by a concurrent grant response.
	credentialRefreshLock sync.Mutex
}

// Option adjusts a Client at construction. The options are test seams that
// mirror Python's opener and https_connection_factory injection.
type Option func(*Client)

// WithOpener replaces the control-plane transport.
func WithOpener(opener Opener) Option {
	return func(client *Client) {
		if opener != nil {
			client.opener = opener
		}
	}
}

// WithSignedTransport replaces the signed object transport.
func WithSignedTransport(transport http.RoundTripper) Option {
	return func(client *Client) {
		if transport != nil {
			client.signed.transport = transport
		}
	}
}

// NewClient validates the configuration and builds both transports.
func NewClient(config Config, options ...Option) (*Client, error) {
	if config.GroupID == "" && config.Workspace == nil {
		return nil, errors.New("group_id or workspace state is required")
	}
	if config.MaxJSONResponseBytes <= 0 {
		return nil, errors.New("max_json_response_bytes must be positive")
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || !validWPSBaseURL(parsed) {
		return nil, errors.New("base_url must be an HTTPS WPS host without a path or credentials")
	}
	suffix := normalizeObjectSuffix(config.ObjectStorageHostSuffix)
	if suffix == "" || (suffix != "kdocs.cn" && !strings.HasSuffix(suffix, ".kdocs.cn")) {
		return nil, errors.New("object_storage_host_suffix must be within kdocs.cn")
	}
	if config.StatusProbeTTL < 0 {
		return nil, errors.New("status_probe_ttl must not be negative")
	}
	if config.StatusFailureBackoff < 0 {
		return nil, errors.New("status_failure_backoff must not be negative")
	}

	client := &Client{
		config: config,
		opener: newControlHTTPClient(config.Timeout),
		signed: NewSignedObjectClient(config),
	}
	for _, option := range options {
		option(client)
	}
	return client, nil
}

// validWPSBaseURL mirrors the urlsplit checks in WpsDriveClient.__init__.
// Empty userinfo is ignored exactly like Python's truthiness check, the raw
// (still percent-encoded) path must be empty or a single slash, and the host
// must be a kdocs.cn host.
func validWPSBaseURL(parsed *url.URL) bool {
	username, password := "", ""
	if parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
	}
	return parsed.Scheme == "https" &&
		parsed.Hostname() != "" &&
		username == "" && password == "" &&
		parsed.RawQuery == "" &&
		parsed.Fragment == "" &&
		(parsed.EscapedPath() == "" || parsed.EscapedPath() == "/") &&
		isWPSHost(parsed.Hostname())
}

// isWPSHost mirrors _is_wps_host: a kdocs.cn host, tolerating trailing dots.
func isWPSHost(host string) bool {
	normalized := strings.ToLower(strings.TrimRight(host, "."))
	return normalized == "kdocs.cn" || strings.HasSuffix(normalized, ".kdocs.cn")
}

// normalizeObjectSuffix mirrors the suffix normalization in client.py:
// surrounding whitespace and dots are removed and the rest is lowercased.
func normalizeObjectSuffix(suffix string) string {
	trimmed := strings.TrimSpace(suffix)
	trimmed = strings.TrimLeft(trimmed, ".")
	trimmed = strings.TrimRight(trimmed, ".")
	return strings.ToLower(trimmed)
}

// hasControlChars reports whether the value carries characters that could
// inject HTTP framing. Python scans string characters; a byte scan matches
// because every control character is ASCII.
func hasControlChars(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] == 0x7F {
			return true
		}
	}
	return false
}

// newControlHTTPClient builds the real control-plane transport: TLS is
// verified, redirects are never followed (the 3xx response surfaces so the
// request layer maps its status), no cookie jar exists, and the configured
// timeout bounds connection phases and the whole bounded control response.
func newControlHTTPClient(timeout float64) *http.Client {
	duration := seconds(timeout)
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: duration, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   duration,
			ResponseHeaderTimeout: duration,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: duration,
	}
}

func seconds(timeout float64) time.Duration {
	return time.Duration(timeout * float64(time.Second))
}
