// The request layer ports client.py's _request_json: one ordered URL and
// query builder, the exact control-plane header set, Set-Cookie persistence
// before any body read, a bounded JSON object response, and a single 401
// retry that re-reads credentials and rewrites the CSRF field in the body.

package wps

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// QueryPair is one ordered query parameter; the wire order matches the
// Python urlencode(list-of-tuples) order.
type QueryPair struct {
	Key   string
	Value string
}

// JSONRequest describes one control-plane JSON call. RetryOn401 is explicit
// because discovery endpoints disable it.
type JSONRequest struct {
	Path       string
	Method     string
	Query      []QueryPair
	Body       []byte
	BaseURL    string
	RetryOn401 bool
}

func (r JSONRequest) method() string {
	if r.Method == "" {
		return http.MethodGet
	}
	return r.Method
}

// RequestJSON mirrors _request_json. HTTP error statuses surface as a
// WpsAPIError carrying the status and the http category without any
// response body content; transport failures carry the unavailable category.
func (c *Client) RequestJSON(request JSONRequest) (map[string]any, error) {
	baseURL := request.BaseURL
	if baseURL == "" {
		baseURL = c.config.BaseURL
	}
	target := buildRequestURL(baseURL, request.Path, request.Query)
	currentCredentials, err := c.currentCredentials()
	if err != nil {
		return nil, err
	}
	currentBody := request.Body
	var response *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		httpRequest, err := newJSONRequest(request.method(), target, currentBody)
		if err != nil {
			return nil, model.NewWpsAPIError(request.Path, 0, model.WpsCategoryUpstream)
		}
		httpRequest.Header.Set("Accept", "*/*")
		if currentBody != nil {
			httpRequest.Header.Set("Content-Type", "application/json")
		}
		if currentCredentials.Cookie != "" {
			httpRequest.Header.Set("Cookie", currentCredentials.Cookie)
		}
		if c.config.Referer != "" {
			httpRequest.Header.Set("Referer", c.config.Referer)
		}
		if c.config.Origin != "" {
			httpRequest.Header.Set("Origin", c.config.Origin)
		}

		opened, err := c.opener.Do(httpRequest)
		if err != nil {
			return nil, model.NewWpsAPIError(request.Path, 0, model.WpsCategoryUnavailable)
		}
		if opened.StatusCode >= 200 && opened.StatusCode <= 299 {
			c.persistSetCookieHeaders(opened.Header)
			response = opened
			break
		}
		rotated := c.persistSetCookieHeaders(opened.Header)
		status := opened.StatusCode
		opened.Body.Close()
		if status == 401 && request.RetryOn401 && attempt == 0 {
			refreshed := rotated
			if !refreshed {
				ok, err := c.refreshCredentials()
				if err != nil {
					return nil, err
				}
				refreshed = ok
			}
			if refreshed {
				newCredentials, err := c.currentCredentials()
				if err != nil {
					return nil, err
				}
				currentCredentials = newCredentials
				currentBody = refreshJSONBody(currentBody, currentCredentials.CSRFToken)
				continue
			}
		}
		return nil, model.NewWpsAPIError(request.Path, status, model.WpsCategoryHTTP)
	}

	if response == nil {
		return nil, model.NewWpsAPIError(request.Path, 401, model.WpsCategoryUpstream)
	}
	payload, err := readLimitedResponse(
		response.Body,
		response.ContentLength,
		c.config.MaxJSONResponseBytes,
		request.Path,
		model.WpsCategoryInvalidResponse,
	)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	decoded, err := decodeJSONObject(payload)
	if err != nil {
		return nil, model.NewWpsAPIError(request.Path, 0, model.WpsCategoryInvalidResponse)
	}
	return decoded, nil
}

func newJSONRequest(method string, target string, body []byte) (*http.Request, error) {
	if body == nil {
		return http.NewRequest(method, target, nil)
	}
	return http.NewRequest(method, target, bytes.NewReader(body))
}

// buildRequestURL mirrors _url: all trailing base slashes and leading path
// slashes collapse into one separator, and the query preserves pair order.
func buildRequestURL(baseURL string, path string, query []QueryPair) string {
	target := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(path, "/")
	encoded := encodeQuery(query)
	if encoded != "" {
		target += "?" + encoded
	}
	return target
}

// encodeQuery mirrors urlencode with quote_plus: order preserved, spaces
// become plus signs, everything outside the unreserved set percent-encoded.
func encodeQuery(query []QueryPair) string {
	parts := make([]string, 0, len(query))
	for _, pair := range query {
		parts = append(parts, url.QueryEscape(pair.Key)+"="+url.QueryEscape(pair.Value))
	}
	return strings.Join(parts, "&")
}

// currentCredentials mirrors _credentials: the source snapshot wins, an
// empty CSRF token falls back to the csrf cookie, and the values must pass
// the outbound header checks before becoming request headers.
func (c *Client) currentCredentials() (credentials.Credentials, error) {
	var current credentials.Credentials
	if c.config.CredentialSource != nil {
		snapshot, err := c.config.CredentialSource.Get()
		if err != nil {
			return credentials.Credentials{}, err
		}
		current = snapshot
	}
	if current.CSRFToken == "" {
		current.CSRFToken = credentials.CSRFFromCookie(current.Cookie)
	}
	if err := credentials.ValidateValues(current); err != nil {
		return credentials.Credentials{}, err
	}
	return current, nil
}

// persistSetCookieHeaders mirrors _persist_set_cookie_headers: store errors
// are swallowed and reported as no rotation.
func (c *Client) persistSetCookieHeaders(headers http.Header) bool {
	if c.config.CredentialSource == nil {
		return false
	}
	stored, err := c.config.CredentialSource.StoreSetCookieHeaders(headers)
	return err == nil && stored
}

// refreshCredentials mirrors _refresh_credentials: refresh grants stay
// serial so a rotated rtk cookie cannot be overwritten by a concurrent
// grant response.
func (c *Client) refreshCredentials() (bool, error) {
	c.credentialRefreshLock.Lock()
	defer c.credentialRefreshLock.Unlock()
	if c.config.CredentialSource != nil {
		refreshed, err := c.config.CredentialSource.Refresh()
		if err != nil {
			return false, err
		}
		if refreshed {
			return true, nil
		}
	}
	if !c.config.AutoRefresh {
		return false, nil
	}
	return c.refreshWPSSession()
}

// accountBaseURL mirrors _account_base_url: the configured account host or
// account.<last two labels> of the API host, both validated as bare HTTPS
// kdocs.cn URLs.
func (c *Client) accountBaseURL() (string, error) {
	base := c.config.AccountBaseURL
	if base == "" {
		parsed, err := url.Parse(c.config.BaseURL)
		hostname := ""
		if err == nil {
			hostname = strings.ToLower(parsed.Hostname())
		}
		labels := []string{}
		if hostname != "" {
			labels = strings.Split(hostname, ".")
		}
		if len(labels) < 2 {
			return "", model.NewWpsAPIError("resolve account refresh URL", 0, model.WpsCategoryUpstream)
		}
		base = "https://account." + labels[len(labels)-2] + "." + labels[len(labels)-1]
	}
	parsed, err := url.Parse(base)
	username, password := "", ""
	if err == nil && parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
	}
	escapedPath := ""
	hostname := ""
	if err == nil {
		escapedPath = parsed.EscapedPath()
		hostname = parsed.Hostname()
	}
	if err != nil ||
		parsed.Scheme != "https" ||
		hostname == "" ||
		username != "" || password != "" ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" ||
		(escapedPath != "" && escapedPath != "/") ||
		!isWPSHost(hostname) {
		return "", model.NewWpsAPIError("resolve account refresh URL", 0, model.WpsCategoryUpstream)
	}
	return strings.TrimRight(base, "/"), nil
}

// refreshWPSSession mirrors _refresh_wps_session: the SDK refresh-token
// grant is attempted with the current cookie and any rotated Set-Cookie is
// persisted. Transport failures report no refresh; a malformed grant body
// fails the enclosing request exactly like the Python helper.
func (c *Client) refreshWPSSession() (bool, error) {
	current, err := c.currentCredentials()
	if err != nil {
		return false, err
	}
	if current.Cookie == "" {
		return false, nil
	}
	baseURL, err := c.accountBaseURL()
	if err != nil {
		return false, err
	}
	httpRequest, err := http.NewRequest(
		http.MethodPost,
		buildRequestURL(baseURL, "/passport/secure/api/grant_token", nil),
		bytes.NewReader([]byte(`{"grant_type":"refresh_token"}`)),
	)
	if err != nil {
		return false, nil
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Cookie", current.Cookie)
	if c.config.Referer != "" {
		httpRequest.Header.Set("Referer", c.config.Referer)
	}
	if c.config.Origin != "" {
		httpRequest.Header.Set("Origin", c.config.Origin)
	}

	opened, err := c.opener.Do(httpRequest)
	if err != nil {
		return false, nil
	}
	ok := opened.StatusCode == http.StatusOK
	headers := opened.Header
	var readErr error
	if ok {
		_, readErr = readLimitedResponse(
			opened.Body,
			opened.ContentLength,
			c.config.MaxJSONResponseBytes,
			"refresh WPS session",
			model.WpsCategoryUpstream,
		)
	}
	opened.Body.Close()
	if !ok {
		return false, nil
	}
	if readErr != nil {
		return false, readErr
	}
	return c.persistSetCookieHeaders(headers), nil
}

// readLimitedResponse mirrors _read_limited_response: a declared oversized
// Content-Length is rejected up front, and the body is read in bounded
// chunks so a lying or stalled server can never exceed the ceiling.
func readLimitedResponse(
	body io.Reader,
	declaredLength int64,
	maxBytes int64,
	operation string,
	errorCategory string,
) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("max_bytes must be positive")
	}
	if declaredLength > maxBytes {
		return nil, model.NewWpsAPIError(operation, 0, errorCategory)
	}
	payload := make([]byte, 0, 4096)
	chunk := make([]byte, 64*1024)
	for {
		limit := int64(len(chunk))
		if remaining := maxBytes + 1 - int64(len(payload)); limit > remaining {
			limit = remaining
		}
		count, readErr := io.ReadFull(body, chunk[:limit])
		payload = append(payload, chunk[:count]...)
		if int64(len(payload)) > maxBytes {
			return nil, model.NewWpsAPIError(operation, 0, errorCategory)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				return payload, nil
			}
			return nil, model.NewWpsAPIError(operation, 0, errorCategory)
		}
	}
}

// decodeJSONObject mirrors json.loads plus the isinstance-dict check: the
// payload must be valid UTF-8, a single JSON value, and an object. Numbers
// stay json.Number so integer fields keep Python's exactness.
func decodeJSONObject(payload []byte) (map[string]any, error) {
	if !utf8.Valid(payload) {
		return nil, errors.New("response is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, errors.New("response is not a JSON object")
	}
	return object, nil
}
