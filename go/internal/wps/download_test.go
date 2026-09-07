// The download fixture tests mirror tests/test_smoke.py's open_download
// coverage: the control request carries credentials, the signed object
// request never does, the observed 403 gains exactly one direct-flag retry,
// range responses are verified against the request, and foreign hosts are
// refused before any object traffic without echoing the URL.

package wps

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// fakeSignedTransport scripts signed-object responses and records every
// request, mirroring the shared Python FakeOpener behavior.
type fakeSignedTransport struct {
	requests []*http.Request
	closed   []bool
	script   []scriptedResponse
	failures []error
}

func (t *fakeSignedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, request)
	if len(t.failures) > 0 {
		err := t.failures[0]
		t.failures = t.failures[1:]
		return nil, err
	}
	if len(t.script) == 0 {
		return nil, errors.New("no scripted object response left")
	}
	next := t.script[0]
	t.script = t.script[1:]
	header := next.header
	if header == nil {
		header = http.Header{}
	}
	tracked := &trackedBody{data: bytes.NewReader(next.body)}
	t.closed = append(t.closed, false)
	index := len(t.closed) - 1
	tracked.onClose = func() { t.closed[index] = true }
	return &http.Response{
		StatusCode:    next.status,
		Status:        fmt.Sprintf("%d %s", next.status, http.StatusText(next.status)),
		Header:        header,
		Body:          tracked,
		ContentLength: int64(len(next.body)),
	}, nil
}

type trackedBody struct {
	data    *bytes.Reader
	onClose func()
}

func (b *trackedBody) Read(p []byte) (int, error) { return b.data.Read(p) }

func (b *trackedBody) Close() error {
	b.onClose()
	return nil
}

const checksumsQueryValue = "md5%2Csha1%2Csha224%2Csha256%2Csha384%2Csha512"

// checksumsQueryPair is the full first resolve query parameter.
const checksumsQueryPair = "support_checksums=" + checksumsQueryValue

func credentialedConfig() Config {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	return config
}

func newDownloadClient(t *testing.T, config Config, opener Opener, transport *fakeSignedTransport) *Client {
	t.Helper()
	client, err := NewClient(config, WithOpener(opener), WithSignedTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func resolveResponse(url string) scriptedResponse {
	return scriptedResponse{status: 200, body: []byte(`{"download_url":"` + url + `","status":"finished"}`)}
}

func objectResponse(status int, header http.Header, body string) scriptedResponse {
	return scriptedResponse{status: status, header: header, body: []byte(body)}
}

func requireWpsAPIError(t *testing.T, err error, operation string, status int) *model.WpsAPIError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a WPS error, got nil")
	}
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok {
		t.Fatalf("expected WpsAPIError, got %T: %v", err, err)
	}
	if apiErr.Operation != operation {
		t.Errorf("operation = %q, want %q", apiErr.Operation, operation)
	}
	if status != 0 && apiErr.Status != status {
		t.Errorf("status = %d, want %d", apiErr.Status, status)
	}
	return apiErr
}

// TestOpenDownloadStreamsSignedObjectWithoutCredentials mirrors
// test_download_stream_does_not_forward_wps_cookie.
func TestOpenDownloadStreamsSignedObjectWithoutCredentials(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		resolveResponse("https://hwc-bj.ag.kdocs.cn/signed?sig=secret"),
	}}
	transport := &fakeSignedTransport{script: []scriptedResponse{
		objectResponse(200, http.Header{
			"Content-Type":   []string{"application/octet-stream"},
			"Content-Length": []string{"12"},
		}, "file-content"),
	}}
	client := newDownloadClient(t, credentialedConfig(), opener, transport)

	stream, err := client.OpenDownload("file-1", 0, nil, model.Ptr("tenant-1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "file-content" {
		t.Errorf("payload = %q", payload)
	}
	if stream.HTTPStatus() != 200 {
		t.Errorf("http status = %d", stream.HTTPStatus())
	}
	if contentType := stream.ContentType(); contentType == nil || *contentType != "application/octet-stream" {
		t.Errorf("content type = %v", stream.ContentType())
	}
	if contentLength := stream.ContentLength(); contentLength == nil || *contentLength != 12 {
		t.Errorf("content length = %v", stream.ContentLength())
	}
	if stream.ContentRange() != nil {
		t.Errorf("unexpected content range %v", stream.ContentRange())
	}
	if status := stream.Status(); status == nil || *status != "finished" {
		t.Errorf("status = %v", stream.Status())
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}

	control := opener.requests[0]
	if control.URL.String() !=
		"https://365.kdocs.cn/api/v3/office/file/file-1/download?"+
			checksumsQueryPair+"&cid=tenant-1" {
		t.Errorf("control URL = %s", control.URL)
	}
	if control.Header.Get("Cookie") != "Cookie-secret" {
		t.Errorf("control cookie = %q", control.Header.Get("Cookie"))
	}

	object := transport.requests[0]
	if object.Method != http.MethodGet {
		t.Errorf("object method = %s", object.Method)
	}
	if object.URL.String() != "https://hwc-bj.ag.kdocs.cn/signed?sig=secret" {
		t.Errorf("object URL = %s", object.URL)
	}
	if object.Header.Get("Cookie") != "" {
		t.Errorf("object request leaked cookie %q", object.Header.Get("Cookie"))
	}
	if object.Header.Get("Accept") != "*/*" {
		t.Errorf("accept = %q", object.Header.Get("Accept"))
	}
	if object.Header.Get("Range") != "" {
		t.Errorf("unexpected range %q", object.Header.Get("Range"))
	}
}

// TestOpenDownloadRetriesWithDirectFlagAfter403 mirrors
// test_download_retries_with_direct_flag_after_observed_403.
func TestOpenDownloadRetriesWithDirectFlagAfter403(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 403, body: []byte(`{"error":"forbidden"}`)},
		resolveResponse("https://hwc-bj.ag.kdocs.cn/signed?sig=secret"),
	}}
	transport := &fakeSignedTransport{script: []scriptedResponse{
		objectResponse(200, nil, "file-content"),
	}}
	client := newDownloadClient(t, credentialedConfig(), opener, transport)

	stream, err := client.OpenDownload("file-1", 0, nil, model.Ptr("file-link-cid"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	if string(payload) != "file-content" {
		t.Errorf("payload = %q", payload)
	}
	if first := opener.requests[0].URL.RawQuery; first != checksumsQueryPair+"&cid=file-link-cid" {
		t.Errorf("first query = %s", first)
	}
	if second := opener.requests[1].URL.RawQuery; second !=
		checksumsQueryPair+"&get_direct_external_download_url=true&cid=file-link-cid" {
		t.Errorf("second query = %s", second)
	}
	if len(opener.requests) != 2 {
		t.Errorf("control requests = %d, want exactly one retry", len(opener.requests))
	}
}

// TestOpenDownloadRangeRequestsPartialObjectResponse mirrors
// test_range_download_requires_partial_object_response.
func TestOpenDownloadRangeRequestsPartialObjectResponse(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		resolveResponse("https://hwc-bj.ag.kdocs.cn/signed"),
	}}
	transport := &fakeSignedTransport{script: []scriptedResponse{
		objectResponse(206, http.Header{
			"Content-Length": []string{"5"},
			"Content-Range":  []string{"bytes 6-10/11"},
		}, "world"),
	}}
	client := newDownloadClient(t, DefaultConfig("group-1"), opener, transport)

	stream, err := client.OpenDownload("file-1", 6, model.Ptr(int64(5)), nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	if string(payload) != "world" {
		t.Errorf("payload = %q", payload)
	}
	if stream.HTTPStatus() != 206 {
		t.Errorf("http status = %d", stream.HTTPStatus())
	}
	if contentRange := stream.ContentRange(); contentRange == nil || *contentRange != "bytes 6-10/11" {
		t.Errorf("content range = %v", stream.ContentRange())
	}
	if rangeHeader := transport.requests[0].Header.Get("Range"); rangeHeader != "bytes=6-10" {
		t.Errorf("range header = %q", rangeHeader)
	}
}

// TestOpenDownloadRejectsMismatchedRangeMetadata mirrors
// test_range_download_rejects_mismatched_content_range.
func TestOpenDownloadRejectsMismatchedRangeMetadata(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		resolveResponse("https://hwc-bj.ag.kdocs.cn/signed"),
	}}
	transport := &fakeSignedTransport{script: []scriptedResponse{
		objectResponse(206, http.Header{
			"Content-Length": []string{"5"},
			"Content-Range":  []string{"bytes 0-4/11"},
		}, "wrong"),
	}}
	client := newDownloadClient(t, DefaultConfig("group-1"), opener, transport)

	_, err := client.OpenDownload("file-1", 6, model.Ptr(int64(5)), nil)
	requireWpsAPIError(t, err, "range response metadata was not honored", 206)
	if !transport.closed[0] {
		t.Errorf("mismatched object response was not closed")
	}
}

// TestOpenDownloadRejectsSignedURLsOutsideObjectStore mirrors
// test_download_rejects_a_signed_url_outside_the_wps_object_store: the URL
// must be refused before any object traffic and never echoed.
func TestOpenDownloadRejectsSignedURLsOutsideObjectStore(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		resolveResponse("https://attacker.example/signed"),
	}}
	transport := &fakeSignedTransport{}
	client := newDownloadClient(t, DefaultConfig("group-1"), opener, transport)

	_, err := client.OpenDownload("file-1", 0, nil, nil)
	requireWpsAPIError(t, err, "resolve download URL", 0)
	message := err.Error()
	if strings.Contains(message, "attacker.example") || strings.Contains(message, "signed") {
		t.Errorf("error leaked the signed URL: %s", message)
	}
	if len(transport.requests) != 0 {
		t.Errorf("object requests = %d, want none", len(transport.requests))
	}
	if len(opener.requests) != 1 {
		t.Errorf("control requests = %d, want one", len(opener.requests))
	}
}

// TestOpenDownloadSignedURLValidation covers allowed and refused signed URL
// shapes end to end: validation happens before the object request and the
// error never carries the URL.
func TestOpenDownloadSignedURLValidation(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		body    []byte // overrides the resolveResponse JSON when set
		allowed bool
	}{
		{"object-host", "https://hwc-bj.ag.kdocs.cn/signed", nil, true},
		{"subdomain", "https://a.b.ag.kdocs.cn/signed", nil, true},
		{"query", "https://hwc-bj.ag.kdocs.cn/signed?x=1", nil, true},
		{"explicit-443", "https://hwc-bj.ag.kdocs.cn:443/signed", nil, true},
		{"foreign-host", "https://attacker.example/signed", nil, false},
		{"lookalike-suffix", "https://ag.kdocs.cn.evil.example/signed", nil, false},
		{"bare-suffix-host", "https://ag.kdocs.cn/signed", nil, true},
		{"http-scheme", "http://hwc-bj.ag.kdocs.cn/signed", nil, false},
		{"userinfo", "https://user:pass@hwc-bj.ag.kdocs.cn/signed", nil, false},
		{"other-port", "https://hwc-bj.ag.kdocs.cn:8443/signed", nil, false},
		{"fragment", "https://hwc-bj.ag.kdocs.cn/signed#fragment", nil, false},
		// The JSON payload escapes the CRLF; decoding hands the real control
		// characters to the URL validator.
		{"control-chars", "https://hwc-bj.ag.kdocs.cn/object\r\nX-Leak: yes",
			[]byte(`{"download_url":"https://hwc-bj.ag.kdocs.cn/object\r\nX-Leak: yes","status":"finished"}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if body == nil {
				body = resolveResponse(tc.url).body
			}
			opener := &fakeControlOpener{script: []scriptedResponse{{status: 200, body: body}}}
			transport := &fakeSignedTransport{script: []scriptedResponse{
				objectResponse(200, nil, "payload"),
			}}
			client := newDownloadClient(t, DefaultConfig("group-1"), opener, transport)

			stream, err := client.OpenDownload("file-1", 0, nil, nil)
			if tc.allowed {
				if err != nil {
					t.Fatal(err)
				}
				stream.Close()
				if len(transport.requests) != 1 {
					t.Errorf("object requests = %d, want one", len(transport.requests))
				}
				return
			}
			apiErr := requireWpsAPIError(t, err, "resolve download URL", 0)
			if strings.Contains(apiErr.Error(), tc.url) {
				t.Errorf("error leaked the signed URL: %s", apiErr.Error())
			}
			if len(transport.requests) != 0 {
				t.Errorf("object requests = %d, want none", len(transport.requests))
			}
		})
	}
}

// TestOpenDownloadAcceptsOnlyURLStrings pins the
// download_url-or-url extraction: any falsy download_url falls through to
// url, while a truthy non-string one fails without falling back.
func TestOpenDownloadAcceptsOnlyURLStrings(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		ok      bool
	}{
		{"download-url", `{"download_url":"https://hwc-bj.ag.kdocs.cn/signed"}`, true},
		{"url-field", `{"url":"https://hwc-bj.ag.kdocs.cn/signed"}`, true},
		{"empty-falls-back", `{"download_url":"","url":"https://hwc-bj.ag.kdocs.cn/signed"}`, true},
		{"null-falls-back", `{"download_url":null,"url":"https://hwc-bj.ag.kdocs.cn/signed"}`, true},
		{"number-no-fallback", `{"download_url":123,"url":"https://hwc-bj.ag.kdocs.cn/signed"}`, false},
		{"relative-url", `{"download_url":"/signed"}`, false},
		{"plain-http-url", `{"download_url":"http://hwc-bj.ag.kdocs.cn/signed"}`, false},
		{"missing", `{}`, false},
		{"null-both", `{"download_url":null,"url":null}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opener := &fakeControlOpener{script: []scriptedResponse{
				{status: 200, body: []byte(tc.payload)},
			}}
			transport := &fakeSignedTransport{script: []scriptedResponse{
				objectResponse(200, nil, "payload"),
			}}
			client := newDownloadClient(t, DefaultConfig("group-1"), opener, transport)

			stream, err := client.OpenDownload("file-1", 0, nil, nil)
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				stream.Close()
				return
			}
			requireWpsAPIError(t, err, "resolve download URL", 0)
			if len(transport.requests) != 0 {
				t.Errorf("object requests = %d, want none", len(transport.requests))
			}
		})
	}
}

// TestOpenDownloadRangeDisabled pins the enable_range gate.
func TestOpenDownloadRangeDisabled(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		resolveResponse("https://hwc-bj.ag.kdocs.cn/signed"),
	}}
	transport := &fakeSignedTransport{script: []scriptedResponse{
		objectResponse(200, nil, "payload"),
	}}
	config := DefaultConfig("group-1")
	config.EnableRange = false
	client := newDownloadClient(t, config, opener, transport)

	_, err := client.OpenDownload("file-1", 6, model.Ptr(int64(5)), nil)
	requireWpsAPIError(t, err, "range download is disabled until independently verified", 0)
	if len(opener.requests) != 0 || len(transport.requests) != 0 {
		t.Errorf("range request ran with enable_range disabled")
	}

	// The default (no offset, no length) download stays available.
	stream, err := client.OpenDownload("file-1", 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
}

// TestOpenDownloadValidatesArguments pins the ValueError surface.
func TestOpenDownloadValidatesArguments(t *testing.T) {
	opener := &fakeControlOpener{}
	transport := &fakeSignedTransport{}
	client := newDownloadClient(t, DefaultConfig("group-1"), opener, transport)

	if _, err := client.OpenDownload("file-1", -1, nil, nil); err == nil ||
		err.Error() != "offset must not be negative" {
		t.Errorf("negative offset error = %v", err)
	}
	if _, err := client.OpenDownload("file-1", 0, model.Ptr(int64(0)), nil); err == nil ||
		err.Error() != "length must be positive" {
		t.Errorf("zero length error = %v", err)
	}
	if len(opener.requests) != 0 || len(transport.requests) != 0 {
		t.Errorf("invalid arguments reached the wire")
	}
}

// TestOpenDownloadRangeRequires206 pins the strict partial-content check.
func TestOpenDownloadRangeRequires206(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		resolveResponse("https://hwc-bj.ag.kdocs.cn/signed"),
	}}
	transport := &fakeSignedTransport{script: []scriptedResponse{
		objectResponse(200, http.Header{"Content-Length": []string{"11"}}, "whole-file"),
	}}
	client := newDownloadClient(t, DefaultConfig("group-1"), opener, transport)

	_, err := client.OpenDownload("file-1", 6, model.Ptr(int64(5)), nil)
	requireWpsAPIError(t, err, "range download was not honored", 200)
	if !transport.closed[0] {
		t.Errorf("non-206 object response was not closed")
	}
}

// TestOpenDownloadFallbackOnlyFor403 pins that only the observed 403 gains
// the direct-flag retry.
func TestOpenDownloadFallbackOnlyFor403(t *testing.T) {
	for _, status := range []int{401, 404, 500} {
		t.Run(fmt.Sprintf("status-%d", status), func(t *testing.T) {
			opener := &fakeControlOpener{script: []scriptedResponse{{status: status}}}
			transport := &fakeSignedTransport{}
			client := newDownloadClient(t, DefaultConfig("group-1"), opener, transport)

			_, err := client.OpenDownload("file-1", 0, nil, nil)
			if err == nil {
				t.Fatalf("status %d did not fail", status)
			}
			if len(opener.requests) != 1 {
				t.Errorf("control requests = %d, want one", len(opener.requests))
			}
			if len(transport.requests) != 0 {
				t.Errorf("object requests = %d, want none", len(transport.requests))
			}
		})
	}
}

// TestOpenDownloadEscapesFileID pins quote(file_id, safe=”) on the
// control path.
func TestOpenDownloadEscapesFileID(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		resolveResponse("https://hwc-bj.ag.kdocs.cn/signed"),
	}}
	transport := &fakeSignedTransport{script: []scriptedResponse{
		objectResponse(200, nil, "payload"),
	}}
	client := newDownloadClient(t, DefaultConfig("group-1"), opener, transport)

	stream, err := client.OpenDownload("a/b c?d", 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	if escaped := opener.requests[0].URL.EscapedPath(); escaped != "/api/v3/office/file/a%2Fb%20c%3Fd/download" {
		t.Errorf("escaped path = %s", escaped)
	}
}

// TestOpenDownloadObjectFailures covers both object-transport failure modes
// and the config cid fallback.
func TestOpenDownloadObjectFailures(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		resolveResponse("https://hwc-bj.ag.kdocs.cn/signed"),
		resolveResponse("https://hwc-bj.ag.kdocs.cn/signed"),
	}}
	transport := &fakeSignedTransport{failures: []error{errors.New("dial refused")}}
	client := newDownloadClient(t, DefaultConfig("group-1"), opener, transport)

	_, err := client.OpenDownload("file-1", 0, nil, nil)
	apiErr := requireWpsAPIError(t, err, "object download", 0)
	if apiErr.Category != model.WpsCategoryUnavailable {
		t.Errorf("transport failure category = %q", apiErr.Category)
	}

	transport = &fakeSignedTransport{script: []scriptedResponse{{status: 403}}}
	client = newDownloadClient(t, DefaultConfig("group-1"), opener, transport)
	_, err = client.OpenDownload("file-1", 0, nil, nil)
	requireWpsAPIError(t, err, "object download", 403)
	if !transport.closed[0] {
		t.Errorf("object error response was not closed")
	}
}

// TestOpenDownloadCIDFalling pins the cid chain: explicit argument, then the
// configured value, then omission.
func TestOpenDownloadCIDFalling(t *testing.T) {
	cases := []struct {
		name       string
		configCID  string
		cid        *string
		wantQuery  string
		wantSuffix string
	}{
		{"explicit", "", model.Ptr("explicit-cid"), checksumsQueryPair, "&cid=explicit-cid"},
		{"config", "config-cid", nil, checksumsQueryPair, "&cid=config-cid"},
		{"override", "config-cid", model.Ptr("explicit-cid"), checksumsQueryPair, "&cid=explicit-cid"},
		{"absent", "", nil, checksumsQueryPair, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opener := &fakeControlOpener{script: []scriptedResponse{
				resolveResponse("https://hwc-bj.ag.kdocs.cn/signed"),
			}}
			transport := &fakeSignedTransport{script: []scriptedResponse{
				objectResponse(200, nil, "payload"),
			}}
			config := DefaultConfig("group-1")
			config.CID = tc.configCID
			client := newDownloadClient(t, config, opener, transport)

			stream, err := client.OpenDownload("file-1", 0, nil, tc.cid)
			if err != nil {
				t.Fatal(err)
			}
			stream.Close()
			rawQuery := opener.requests[0].URL.RawQuery
			if rawQuery != tc.wantQuery+tc.wantSuffix {
				t.Errorf("query = %s, want %s%s", rawQuery, tc.wantQuery, tc.wantSuffix)
			}
		})
	}
}

// TestRangeResponseMatches mirrors _range_response_matches, including the
// oversized-digit behavior Python's unbounded integers provide.
func TestRangeResponseMatches(t *testing.T) {
	cases := []struct {
		name          string
		value         *string
		offset        int64
		length        *int64
		contentLength *int64
		want          bool
	}{
		{"match", model.Ptr("bytes 6-10/11"), 6, model.Ptr(int64(5)), model.Ptr(int64(5)), true},
		{"stripped", model.Ptr("  bytes 6-10/11  "), 6, model.Ptr(int64(5)), model.Ptr(int64(5)), true},
		{"unknown-total", model.Ptr("bytes 6-10/*"), 6, model.Ptr(int64(5)), model.Ptr(int64(5)), true},
		{"total-above-end", model.Ptr("bytes 6-10/20"), 6, model.Ptr(int64(5)), model.Ptr(int64(5)), true},
		{"total-below-end", model.Ptr("bytes 6-10/5"), 6, model.Ptr(int64(5)), model.Ptr(int64(5)), false},
		{"wrong-start", model.Ptr("bytes 0-4/11"), 6, model.Ptr(int64(5)), model.Ptr(int64(5)), false},
		{"reversed", model.Ptr("bytes 10-6/11"), 6, model.Ptr(int64(5)), model.Ptr(int64(5)), false},
		{"covered-mismatch", model.Ptr("bytes 6-10/11"), 6, model.Ptr(int64(5)), model.Ptr(int64(11)), false},
		{"length-mismatch", model.Ptr("bytes 6-10/11"), 6, model.Ptr(int64(4)), model.Ptr(int64(5)), false},
		{"open-request", model.Ptr("bytes 6-10/11"), 6, nil, model.Ptr(int64(5)), true},
		{"nil-value", nil, 6, model.Ptr(int64(5)), model.Ptr(int64(5)), false},
		{"empty-value", model.Ptr(""), 6, model.Ptr(int64(5)), model.Ptr(int64(5)), false},
		{"blank-value", model.Ptr("   "), 6, model.Ptr(int64(5)), model.Ptr(int64(5)), false},
		{"not-bytes", model.Ptr("items 6-10/11"), 6, model.Ptr(int64(5)), model.Ptr(int64(5)), false},
		{"missing-total", model.Ptr("bytes 6-10"), 6, model.Ptr(int64(5)), model.Ptr(int64(5)), false},
		{"nil-content-length", model.Ptr("bytes 6-10/11"), 6, model.Ptr(int64(5)), nil, false},
		{"negative-content-length", model.Ptr("bytes 6-10/11"), 6, model.Ptr(int64(5)), model.Ptr(int64(-5)), false},
		{"zero-length", model.Ptr("bytes 6-5/0"), 6, nil, model.Ptr(int64(0)), false},
		{"ceiling-overflow", model.Ptr("bytes 0-9223372036854775807/9223372036854775807"), 0, nil, model.Ptr(int64(1 << 62)), false},
		{"huge-start", model.Ptr("bytes 99999999999999999999999-1000/1"), 0, nil, model.Ptr(int64(1)), false},
		{"huge-total", model.Ptr("bytes 0-0/99999999999999999999999"), 0, model.Ptr(int64(1)), model.Ptr(int64(1)), true},
		{"huge-end", model.Ptr("bytes 0-99999999999999999999999/1"), 0, nil, model.Ptr(int64(1)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rangeResponseMatches(tc.value, tc.offset, tc.length, tc.contentLength); got != tc.want {
				t.Errorf("rangeResponseMatches(%v, %d) = %v, want %v", tc.value, tc.offset, got, tc.want)
			}
		})
	}
}

// TestHeaderInt mirrors the int(headers.get(...)) tolerance.
func TestHeaderInt(t *testing.T) {
	cases := []struct {
		raw  string
		want *int64
	}{
		{"12", model.Ptr(int64(12))},
		{" 12 ", model.Ptr(int64(12))},
		{"+5", model.Ptr(int64(5))},
		{"-5", model.Ptr(int64(-5))},
		{"", nil},
		{"abc", nil},
		{"5.0", nil},
	}
	for _, tc := range cases {
		header := http.Header{"X-Value": []string{tc.raw}}
		got := headerInt(header, "X-Value")
		if tc.want == nil && got != nil {
			t.Errorf("headerInt(%q) = %v, want nil", tc.raw, *got)
			continue
		}
		if tc.want != nil && (got == nil || *got != *tc.want) {
			t.Errorf("headerInt(%q) = %v, want %d", tc.raw, got, *tc.want)
		}
	}
	if got := headerInt(http.Header{}, "X-Value"); got != nil {
		t.Errorf("missing header = %v, want nil", *got)
	}
}
