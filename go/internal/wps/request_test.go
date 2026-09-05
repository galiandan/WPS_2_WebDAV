package wps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// fakeControlOpener scripts control-plane responses and records every
// request and body that would hit the wire.
type fakeControlOpener struct {
	requests       []*http.Request
	bodies         [][]byte
	script         []scriptedResponse
	failures       []error
	declaredLength int64 // overrides Content-Length when positive
}

type scriptedResponse struct {
	status int
	header http.Header
	body   []byte
}

func (o *fakeControlOpener) Do(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
		request.Body.Close()
	}
	o.requests = append(o.requests, request)
	o.bodies = append(o.bodies, body)
	if len(o.failures) > 0 {
		err := o.failures[0]
		o.failures = o.failures[1:]
		return nil, err
	}
	if len(o.script) == 0 {
		return nil, errors.New("no scripted response left")
	}
	next := o.script[0]
	o.script = o.script[1:]
	header := next.header
	if header == nil {
		header = http.Header{}
	}
	declared := int64(len(next.body))
	if o.declaredLength > 0 {
		declared = o.declaredLength
	}
	return &http.Response{
		StatusCode:    next.status,
		Status:        fmt.Sprintf("%d %s", next.status, http.StatusText(next.status)),
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(next.body)),
		ContentLength: declared,
	}, nil
}

func staticSource() *credentials.StaticCredentialSource {
	return &credentials.StaticCredentialSource{
		Credentials: credentials.Credentials{Cookie: "Cookie-secret", CSRFToken: "csrf-secret"},
	}
}

func TestRequestJSONBuildsURLQueryAndHeaders(t *testing.T) {
	config := DefaultConfig("group-1")
	config.Referer = "https://365.kdocs.cn/app"
	config.Origin = "https://365.kdocs.cn"
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok"}`)},
	}}
	client.opener = opener

	payload, err := client.RequestJSON(JSONRequest{
		Path:  "/api/v3/islogin",
		Query: []QueryPair{{Key: "b", Value: "2"}, {Key: "a", Value: "1"}},
	})
	if err != nil {
		t.Fatalf("RequestJSON failed: %v", err)
	}
	if payload["result"] != "ok" {
		t.Fatalf("payload = %v, want result ok", payload)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}
	request := opener.requests[0]
	want := "https://365.kdocs.cn/api/v3/islogin?b=2&a=1"
	if request.URL.String() != want {
		t.Fatalf("URL = %q, want %q", request.URL.String(), want)
	}
	if request.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET", request.Method)
	}
	if request.Header.Get("Accept") != "*/*" {
		t.Fatalf("Accept = %q, want */*", request.Header.Get("Accept"))
	}
	if request.Header.Get("Cookie") != "Cookie-secret" {
		t.Fatalf("Cookie = %q", request.Header.Get("Cookie"))
	}
	if request.Header.Get("Referer") != "https://365.kdocs.cn/app" ||
		request.Header.Get("Origin") != "https://365.kdocs.cn" {
		t.Fatalf("Referer/Origin = %q/%q", request.Header.Get("Referer"), request.Header.Get("Origin"))
	}
	if request.Header.Get("Content-Type") != "" {
		t.Fatalf("bodyless GET must not carry Content-Type")
	}
	if request.Header.Get("User-Agent") != "" {
		t.Fatalf("request builder must not add a browser mimicry User-Agent")
	}
}

func TestRequestJSONPostsJSONBody(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"id":2}`)},
	}}
	client.opener = opener

	if _, err := client.RequestJSON(JSONRequest{
		Path:   "/3rd/drive/api/v5/files/folder",
		Method: http.MethodPost,
		Body:   []byte(`{"groupid":1,"name":"ok"}`),
	}); err != nil {
		t.Fatalf("RequestJSON failed: %v", err)
	}
	request := opener.requests[0]
	if request.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", request.Method)
	}
	if request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", request.Header.Get("Content-Type"))
	}
	if string(opener.bodies[0]) != `{"groupid":1,"name":"ok"}` {
		t.Fatalf("body = %q", opener.bodies[0])
	}
}

func TestBuildRequestURLAndQueryEncoding(t *testing.T) {
	if got := buildRequestURL("https://host/", "/p", nil); got != "https://host/p" {
		t.Fatalf("URL = %q", got)
	}
	if got := buildRequestURL("https://host///", "p", nil); got != "https://host/p" {
		t.Fatalf("URL = %q", got)
	}
	if got := buildRequestURL("https://host", "", nil); got != "https://host/" {
		t.Fatalf("URL = %q", got)
	}
	got := encodeQuery([]QueryPair{
		{Key: "k", Value: "a b/c中文"},
		{Key: "z", Value: ""},
		{Key: "k", Value: "two"},
	})
	if got != "k=a+b%2Fc%E4%B8%AD%E6%96%87&z=&k=two" {
		t.Fatalf("query = %q", got)
	}
	parsed, err := url.ParseQuery(got)
	if err != nil {
		t.Fatalf("encoded query must stay parseable: %v", err)
	}
	if strings.Join(parsed["k"], ",") != "a b/c中文,two" {
		t.Fatalf("parsed k = %v", parsed["k"])
	}
}

func TestRequestJSONRejectsNonObjectResponses(t *testing.T) {
	bodies := [][]byte{
		[]byte(`[]`),
		[]byte(`42`),
		[]byte(`"x"`),
		[]byte(`null`),
		[]byte(``),
		[]byte(`not json`),
		[]byte(`{"a":1} trailing`),
		{0xff, 0xfe, 0x00, 0x01},
	}
	for _, body := range bodies {
		config := DefaultConfig("group-1")
		config.CredentialSource = staticSource()
		client, err := NewClient(config)
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		opener := &fakeControlOpener{script: []scriptedResponse{{status: 200, body: body}}}
		client.opener = opener

		_, err = client.RequestJSON(JSONRequest{Path: "/api/x"})
		apiErr, ok := model.AsWpsAPIError(err)
		if !ok || apiErr.Operation != "/api/x" || apiErr.Status != 0 ||
			apiErr.Category != model.WpsCategoryInvalidResponse {
			t.Fatalf("body %q error = %v, want invalid_response without status", body, err)
		}
	}
}

func TestRequestJSONEnforcesMaxBytes(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	config.MaxJSONResponseBytes = 64
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	exact := []byte(`{"a":"` + strings.Repeat("a", 56) + `"}`)
	if len(exact) != 64 {
		t.Fatalf("fixture length = %d, want 64", len(exact))
	}
	opener := &fakeControlOpener{script: []scriptedResponse{{status: 200, body: exact}}}
	client.opener = opener
	if _, err := client.RequestJSON(JSONRequest{Path: "/api/x"}); err != nil {
		t.Fatalf("exact-size JSON rejected: %v", err)
	}

	oversize := append([]byte{}, exact...)
	oversize = append(oversize, ' ')
	opener = &fakeControlOpener{script: []scriptedResponse{{status: 200, body: oversize}}}
	client.opener = opener
	_, err = client.RequestJSON(JSONRequest{Path: "/api/x"})
	if apiErr, ok := model.AsWpsAPIError(err); !ok ||
		apiErr.Category != model.WpsCategoryInvalidResponse {
		t.Fatalf("oversized body error = %v, want invalid_response", err)
	}

	opener = &fakeControlOpener{
		script:         []scriptedResponse{{status: 200, body: []byte(`{}`)}},
		declaredLength: 1 << 20,
	}
	client.opener = opener
	_, err = client.RequestJSON(JSONRequest{Path: "/api/x"})
	if apiErr, ok := model.AsWpsAPIError(err); !ok ||
		apiErr.Category != model.WpsCategoryInvalidResponse {
		t.Fatalf("declared oversize error = %v, want invalid_response", err)
	}
}

func TestRequestJSONMapsHTTPStatusWithoutBody(t *testing.T) {
	for _, test := range []struct {
		status int
		want   string
	}{
		{403, "WPS operation failed: /api/x (HTTP 403)"},
		{302, "WPS operation failed: /api/x (HTTP 302)"},
		{500, "WPS operation failed: /api/x (HTTP 500)"},
	} {
		config := DefaultConfig("group-1")
		config.CredentialSource = staticSource()
		client, err := NewClient(config)
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		opener := &fakeControlOpener{script: []scriptedResponse{{
			status: test.status,
			body:   []byte(`{"error":"upstream-secret"}`),
		}}}
		client.opener = opener

		_, err = client.RequestJSON(JSONRequest{Path: "/api/x"})
		apiErr, ok := model.AsWpsAPIError(err)
		if !ok || apiErr.Status != test.status || apiErr.Category != model.WpsCategoryHTTP {
			t.Fatalf("status %d error = %v, want http category with status", test.status, err)
		}
		if err.Error() != test.want {
			t.Fatalf("message = %q, want %q", err.Error(), test.want)
		}
		if strings.Contains(err.Error(), "upstream-secret") {
			t.Fatalf("error message leaks the response body: %q", err.Error())
		}
	}
}

func TestRequestJSONTransportFailureIsUnavailable(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{failures: []error{errors.New("connection refused")}}
	client.opener = opener

	_, err = client.RequestJSON(JSONRequest{Path: "/api/x"})
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Status != 0 || apiErr.Category != model.WpsCategoryUnavailable {
		t.Fatalf("error = %v, want unavailable without status", err)
	}
	if err.Error() != "WPS operation failed: /api/x" {
		t.Fatalf("message = %q, want the operation-only message", err.Error())
	}
}

func writeCredentialFiles(t *testing.T, cookieText string, csrfText string) (string, *credentials.FileCredentialSource) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("chmod fixture dir failed: %v", err)
	}
	cookiePath := filepath.Join(directory, "cookie")
	csrfPath := filepath.Join(directory, "csrf")
	if err := os.WriteFile(cookiePath, []byte(cookieText), 0o600); err != nil {
		t.Fatalf("write cookie fixture failed: %v", err)
	}
	if err := os.Chmod(cookiePath, 0o600); err != nil {
		t.Fatalf("chmod cookie fixture failed: %v", err)
	}
	if err := os.WriteFile(csrfPath, []byte(csrfText), 0o600); err != nil {
		t.Fatalf("write csrf fixture failed: %v", err)
	}
	if err := os.Chmod(csrfPath, 0o600); err != nil {
		t.Fatalf("chmod csrf fixture failed: %v", err)
	}
	return directory, &credentials.FileCredentialSource{
		CookiePath:    cookiePath,
		CSRFTokenPath: csrfPath,
	}
}

func TestRequestJSON401RetriesWithRotatedFileCredentials(t *testing.T) {
	directory, source := writeCredentialFiles(t, "sid=first", "csrf-first")
	config := DefaultConfig("group-1")
	config.CredentialSource = source
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 401},
		{status: 200, body: []byte(`{"id":2,"result":"ok"}`)},
	}}
	// The first open rotates the credential files behind the adapter's back,
	// exactly like the Python fixture.
	rotate := func() {
		if err := os.WriteFile(filepath.Join(directory, "cookie"), []byte("sid=second"), 0o600); err != nil {
			t.Fatalf("rotate cookie failed: %v", err)
		}
		if err := os.WriteFile(filepath.Join(directory, "csrf"), []byte("csrf-second"), 0o600); err != nil {
			t.Fatalf("rotate csrf failed: %v", err)
		}
	}
	client.opener = &rotatingOpener{inner: opener, before: rotate}

	body := []byte(`{"groupid":1,"csrfmiddlewaretoken":"csrf-first","name":"x"}`)
	payload, err := client.RequestJSON(JSONRequest{
		Path:       "/3rd/drive/api/v5/files/folder",
		Method:     http.MethodPost,
		Body:       body,
		RetryOn401: true,
	})
	if err != nil {
		t.Fatalf("RequestJSON failed: %v", err)
	}
	if payload["id"] != json.Number("2") {
		t.Fatalf("payload = %v", payload)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(opener.requests))
	}
	retry := opener.requests[1]
	if retry.Header.Get("Cookie") != "sid=second" {
		t.Fatalf("retry Cookie = %q, want sid=second", retry.Header.Get("Cookie"))
	}
	wantBody := `{"groupid":1,"csrfmiddlewaretoken":"csrf-second","name":"x"}`
	if string(opener.bodies[1]) != wantBody {
		t.Fatalf("retry body = %q, want %q", opener.bodies[1], wantBody)
	}
}

// rotatingOpener runs a hook before each response so fixtures can rotate
// credential files mid-request the way the Python tests do.
type rotatingOpener struct {
	inner  *fakeControlOpener
	before func()
}

func (o *rotatingOpener) Do(request *http.Request) (*http.Response, error) {
	if len(o.inner.requests) == 0 && o.before != nil {
		o.before()
	}
	return o.inner.Do(request)
}

func TestRequestJSON401GrantRefreshAndPersist(t *testing.T) {
	_, source := writeCredentialFiles(t, "sid=first; rtk=refresh-ticket", "csrf-first")
	config := DefaultConfig("group-1")
	config.CredentialSource = source
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 401},
		{status: 200, body: []byte(`{"result":"ok"}`), header: http.Header{
			"Set-Cookie": []string{"sid=second; Path=/", "csrf=csrf-second; Path=/"},
		}},
		{status: 200, body: []byte(`{"files":[],"next_offset":-1,"result":"ok"}`)},
	}}
	client.opener = opener

	if _, err := client.RequestJSON(JSONRequest{
		Path:       "/3rd/drive/api/v5/files",
		Query:      []QueryPair{{Key: "parentid", Value: "folder-1"}},
		RetryOn401: true,
	}); err != nil {
		t.Fatalf("RequestJSON failed: %v", err)
	}
	if len(opener.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(opener.requests))
	}
	grant := opener.requests[1]
	if grant.URL.Path != "/passport/secure/api/grant_token" ||
		grant.URL.Host != "account.kdocs.cn" {
		t.Fatalf("grant URL = %v, want account.kdocs.cn grant_token", grant.URL)
	}
	if string(opener.bodies[1]) != `{"grant_type":"refresh_token"}` {
		t.Fatalf("grant body = %q", opener.bodies[1])
	}
	if grant.Header.Get("Accept") != "application/json" ||
		grant.Header.Get("Cookie") != "sid=first; rtk=refresh-ticket" {
		t.Fatalf("grant headers = %v", grant.Header)
	}
	retry := opener.requests[2]
	if retry.Header.Get("Cookie") != "sid=second; rtk=refresh-ticket; csrf=csrf-second" {
		t.Fatalf("retry Cookie = %q", retry.Header.Get("Cookie"))
	}
	cookieText, err := os.ReadFile(source.CookiePath)
	if err != nil || strings.TrimSpace(string(cookieText)) != "sid=second; rtk=refresh-ticket; csrf=csrf-second" {
		t.Fatalf("cookie file = %q err = %v", cookieText, err)
	}
	csrfText, err := os.ReadFile(source.CSRFTokenPath)
	if err != nil || strings.TrimSpace(string(csrfText)) != "csrf-second" {
		t.Fatalf("csrf file = %q err = %v", csrfText, err)
	}
}

func TestRequestJSON401WithoutRetrySurfaces401(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{{status: 401}}}
	client.opener = opener

	_, err = client.RequestJSON(JSONRequest{Path: "/api/x"})
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Status != 401 || apiErr.Category != model.WpsCategoryHTTP {
		t.Fatalf("error = %v, want 401 http", err)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}

	config.AutoRefresh = false
	client, err = NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener = &fakeControlOpener{script: []scriptedResponse{{status: 401}}}
	client.opener = opener
	_, err = client.RequestJSON(JSONRequest{Path: "/api/x", RetryOn401: true})
	apiErr, ok = model.AsWpsAPIError(err)
	if !ok || apiErr.Status != 401 || apiErr.Category != model.WpsCategoryHTTP {
		t.Fatalf("error = %v, want 401 http without autorefresh", err)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}
}

func TestRequestJSON401RetriesOnlyOnce(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 401},
		{status: 401},
	}}
	client.opener = opener

	_, err = client.RequestJSON(JSONRequest{Path: "/api/x", RetryOn401: true})
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Status != 401 || apiErr.Category != model.WpsCategoryHTTP {
		t.Fatalf("error = %v, want 401 http after retry", err)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(opener.requests))
	}
}

func TestRequestJSONPersistsSetCookieBeforeReadFailure(t *testing.T) {
	_, source := writeCredentialFiles(t, "sid=first", "csrf-first")
	config := DefaultConfig("group-1")
	config.CredentialSource = source
	config.MaxJSONResponseBytes = 8
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{{
		status: 200,
		header: http.Header{"Set-Cookie": []string{"sid=rotated; Path=/"}},
		body:   []byte(`{"way":"too large"}`),
	}}}
	client.opener = opener

	_, err = client.RequestJSON(JSONRequest{Path: "/api/x"})
	if apiErr, ok := model.AsWpsAPIError(err); !ok ||
		apiErr.Category != model.WpsCategoryInvalidResponse {
		t.Fatalf("error = %v, want invalid_response", err)
	}
	cookieText, err := os.ReadFile(source.CookiePath)
	if err != nil || strings.TrimSpace(string(cookieText)) != "sid=rotated" {
		t.Fatalf("cookie file = %q err = %v, want rotation persisted before the read failed", cookieText, err)
	}
}

func TestAccountBaseURLDerivationAndValidation(t *testing.T) {
	config := DefaultConfig("group-1")
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	got, err := client.accountBaseURL()
	if err != nil || got != "https://account.kdocs.cn" {
		t.Fatalf("accountBaseURL = %q err = %v, want https://account.kdocs.cn", got, err)
	}

	config = DefaultConfig("group-1")
	config.AccountBaseURL = "http://account.kdocs.cn"
	client, err = NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	if _, err := client.accountBaseURL(); err == nil ||
		err.Error() != "WPS operation failed: resolve account refresh URL" {
		t.Fatalf("error = %v, want the fixed account URL message", err)
	}

	config.AccountBaseURL = "https://account.kdocs.cn/passport"
	client, err = NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	if _, err := client.accountBaseURL(); err == nil {
		t.Fatal("account URL with a path must be rejected")
	}

	config.AccountBaseURL = "https://attacker.example/"
	client, err = NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	if _, err := client.accountBaseURL(); err == nil {
		t.Fatal("non-kdocs account URL must be rejected")
	}
}
