package wps

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// recordingTransport stands in for the signed object transport and records
// every request it would put on the wire.
type recordingTransport struct {
	requests []*http.Request
	response *http.Response
	err      error
}

func (t *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, request)
	if t.err != nil {
		return nil, t.err
	}
	return t.response, nil
}

func cannedResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Etag": {`"fake-etag"`}},
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}
}

func signedTestClient(t *testing.T) (*Client, *recordingTransport, credentials.Credentials) {
	t.Helper()
	config := DefaultConfig("group-1")
	config.CredentialSource = &credentials.StaticCredentialSource{
		Credentials: credentials.Credentials{Cookie: "Cookie-secret", CSRFToken: "csrf-secret"},
	}
	snapshot, err := config.CredentialSource.Get()
	if err != nil || snapshot.Cookie == "" || snapshot.CSRFToken == "" {
		t.Fatalf("credential source must hold values for the isolation proof: %v", err)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	transport := &recordingTransport{response: cannedResponse()}
	client.signed.transport = transport
	return client, transport, snapshot
}

func TestParseSignedTargetValidShapes(t *testing.T) {
	tests := []struct {
		name      string
		signedURL string
		want      SignedTarget
	}{
		{
			name:      "signed download url",
			signedURL: "https://hwc-bj.ag.kdocs.cn/signed?sig=fake-signature",
			want:      SignedTarget{Host: "hwc-bj.ag.kdocs.cn", Port: 0, Target: "/signed?sig=fake-signature"},
		},
		{
			name:      "explicit default port",
			signedURL: "https://hwc-bj.ag.kdocs.cn:443/x",
			want:      SignedTarget{Host: "hwc-bj.ag.kdocs.cn", Port: 443, Target: "/x"},
		},
		{
			name:      "leading zero port equals 443 like Python int parsing",
			signedURL: "https://hwc-bj.ag.kdocs.cn:0443/x",
			want:      SignedTarget{Host: "hwc-bj.ag.kdocs.cn", Port: 443, Target: "/x"},
		},
		{
			name:      "bare host",
			signedURL: "https://kdocs.cn",
			want:      SignedTarget{Host: "kdocs.cn", Port: 0, Target: "/"},
		},
		{
			name:      "raw path and query preserved",
			signedURL: "https://HWC-BJ.AG.KDOCS.CN./a%2Fb?q=%2F",
			want:      SignedTarget{Host: "hwc-bj.ag.kdocs.cn.", Port: 0, Target: "/a%2Fb?q=%2F"},
		},
		{
			name:      "suffix argument is normalized",
			signedURL: "https://hwc-bj.ag.kdocs.cn/x",
			want:      SignedTarget{Host: "hwc-bj.ag.kdocs.cn", Port: 0, Target: "/x"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, err := ParseSignedTarget(test.signedURL, "signed URL", "KDOCS.CN")
			if err != nil {
				t.Fatalf("ParseSignedTarget(%q) failed: %v", test.signedURL, err)
			}
			if target != test.want {
				t.Fatalf("target = %+v, want %+v", target, test.want)
			}
		})
	}
}

func TestParseSignedTargetRejects(t *testing.T) {
	tests := []struct {
		name      string
		signedURL string
	}{
		{"control characters", "https://hwc-bj.ag.kdocs.cn/object\r\nX-Leak: yes"},
		{"plain http", "http://hwc-bj.ag.kdocs.cn/signed"},
		{"outside object store", "https://attacker.example/signed"},
		{"suffix lookalike", "https://kdocs.cn.evil.example/signed"},
		{"userinfo", "https://user:pass@hwc-bj.ag.kdocs.cn/signed"},
		{"fragment", "https://hwc-bj.ag.kdocs.cn/signed#f"},
		{"foreign port", "https://hwc-bj.ag.kdocs.cn:8443/signed"},
		{"port zero", "https://hwc-bj.ag.kdocs.cn:0/signed"},
		{"invalid port", "https://hwc-bj.ag.kdocs.cn:bad/signed"},
		{"empty url", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, err := ParseSignedTarget(test.signedURL, "signed URL", DefaultObjectStorageHostSuffix)
			if err == nil {
				t.Fatalf("ParseSignedTarget(%q) = %+v, want rejection", test.signedURL, target)
			}
			apiErr, ok := model.AsWpsAPIError(err)
			if !ok {
				t.Fatalf("error = %v, want *model.WpsAPIError", err)
			}
			if apiErr.Operation != "signed URL" || apiErr.Status != 0 || apiErr.Category != model.WpsCategoryUpstream {
				t.Fatalf("error fields = %+v, want operation-only upstream error", apiErr)
			}
			if strings.Contains(err.Error(), "hwc-bj") {
				t.Fatalf("error message must not echo the signed URL: %q", err.Error())
			}
		})
	}
}

func TestSignedClientDoRejectsCredentialHeaders(t *testing.T) {
	client, transport, _ := signedTestClient(t)
	for _, name := range []string{"Cookie", "cookie", "Authorization", "AUTHORIZATION", "X-Csrf-Token", "csrf-token"} {
		_, err := client.signed.Do(
			"object download",
			http.MethodGet,
			"https://hwc-bj.ag.kdocs.cn/signed",
			[]SignedHeader{{Name: name, Value: "fake-value"}},
			nil,
			0,
		)
		if err == nil {
			t.Fatalf("header %q accepted", name)
		}
		apiErr, ok := model.AsWpsAPIError(err)
		if !ok || apiErr.Operation != "object download" || apiErr.Category != model.WpsCategoryUpstream {
			t.Fatalf("header %q produced %v, want an upstream WpsAPIError", name, err)
		}
	}
	if len(transport.requests) != 0 {
		t.Fatalf("transport called %d times, want 0", len(transport.requests))
	}
}

// TestSignedClientSendsOnlyExplicitHeaders is the B400 completion fixture:
// the client holds real credential values, yet the request that would reach
// the signed object host carries none of them.
func TestSignedClientSendsOnlyExplicitHeaders(t *testing.T) {
	client, transport, snapshot := signedTestClient(t)

	response, err := client.signed.Do(
		"object download",
		http.MethodGet,
		"https://hwc-bj.ag.kdocs.cn/signed?sig=fake-signature",
		[]SignedHeader{{Name: "Accept", Value: "*/*"}, {Name: "Range", Value: "bytes=0-4"}},
		nil,
		0,
	)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if len(transport.requests) != 1 {
		t.Fatalf("requests recorded = %d, want 1", len(transport.requests))
	}
	request := transport.requests[0]
	if request.URL.Host != "hwc-bj.ag.kdocs.cn" || request.URL.Path != "/signed" ||
		request.URL.RawQuery != "sig=fake-signature" {
		t.Fatalf("request URL = %v, want the signed object host", request.URL)
	}
	if len(request.Header) != 2 ||
		request.Header.Get("Accept") != "*/*" || request.Header.Get("Range") != "bytes=0-4" {
		t.Fatalf("headers = %v, want only Accept and Range", request.Header)
	}
	for name := range request.Header {
		lowered := strings.ToLower(name)
		if lowered == "cookie" || lowered == "authorization" || strings.Contains(lowered, "csrf") {
			t.Fatalf("credential header %q reached the signed transport", name)
		}
	}
	_ = snapshot

	uploadBody := []byte("file-body")
	response, err = client.signed.Do(
		"object upload",
		http.MethodPut,
		"https://hwc-bj.ag.kdocs.cn:443/object-1",
		[]SignedHeader{{Name: "Content-Type", Value: "application/octet-stream"}},
		bytes.NewReader(uploadBody),
		int64(len(uploadBody)),
	)
	if err != nil {
		t.Fatalf("Do upload failed: %v", err)
	}
	defer response.Body.Close()
	request = transport.requests[1]
	if request.Method != http.MethodPut || request.ContentLength != int64(len(uploadBody)) {
		t.Fatalf("method/content length = %s/%d, want PUT/9", request.Method, request.ContentLength)
	}
	if request.Host != "hwc-bj.ag.kdocs.cn" {
		t.Fatalf("Host = %q, want the bare host without the default port", request.Host)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil || string(body) != "file-body" {
		t.Fatalf("body = %q err = %v, want file-body", body, err)
	}
	if len(request.Header) != 1 || request.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("headers = %v, want only Content-Type", request.Header)
	}
}

func TestSignedClientWrapsTransportErrorsWithoutURL(t *testing.T) {
	client, transport, _ := signedTestClient(t)
	transport.err = &url.Error{
		Op:  "Get",
		URL: "https://hwc-bj.ag.kdocs.cn/signed?sig=fake-signature",
		Err: errors.New("connection reset"),
	}

	_, err := client.signed.Do(
		"object download",
		http.MethodGet,
		"https://hwc-bj.ag.kdocs.cn/signed?sig=fake-signature",
		nil,
		nil,
		0,
	)
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok {
		t.Fatalf("error = %v, want *model.WpsAPIError", err)
	}
	if apiErr.Operation != "object download" || apiErr.Status != 0 ||
		apiErr.Category != model.WpsCategoryUnavailable {
		t.Fatalf("error fields = %+v, want unavailable without status", apiErr)
	}
	message := err.Error()
	if strings.Contains(message, "sig=") || strings.Contains(message, "hwc-bj") {
		t.Fatalf("error message leaks the signed URL: %q", message)
	}
}

func TestSignedTransportVerifiesTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	if _, err := newSignedTransport(30).RoundTrip(request); err == nil ||
		!strings.Contains(err.Error(), "certificate") {
		t.Fatalf("error = %v, want a certificate verification failure", err)
	}
}

func TestSignedTransportDialsDirectlyWithBoundedPhases(t *testing.T) {
	transport, ok := newSignedTransport(2.5).(*http.Transport)
	if !ok {
		t.Fatalf("signed transport is %T, want *http.Transport", transport)
	}
	if transport.Proxy != nil {
		t.Fatal("signed transport must dial directly like the Python raw connection")
	}
	if transport.TLSHandshakeTimeout != 2500*time.Millisecond ||
		transport.ResponseHeaderTimeout != 2500*time.Millisecond || transport.DialContext == nil {
		t.Fatalf("phase timeouts not applied: %+v", transport)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("signed transport must pin TLS 1.2 as the minimum version")
	}
}
