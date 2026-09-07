// The Range tests follow the B802 plan: table-driven parser cases first,
// then the wire behavior against values captured from the live Python
// reference server (head branch included). The fixtures mirror
// tests/test_server.py's FakeStorage with stream_chunk_size-aware fakes.

package httpserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// TestParseRangeHeader covers closed, open, and suffix ranges, clamping,
// empty files, unknown sizes, multi-range and unit rejections, malformed
// specs, and the huge-digit behavior of Python's unbounded int.
func TestParseRangeHeader(t *testing.T) {
	eleven := int64(11)
	zero := int64(0)
	cases := []struct {
		name   string
		value  string
		size   *int64
		start  int64
		length int64
		fail   bool
	}{
		{"closed", "bytes=6-10", &eleven, 6, 5, false},
		{"closed-full", "bytes=0-10", &eleven, 0, 11, false},
		{"open", "bytes=6-", &eleven, 6, 5, false},
		{"open-from-zero", "bytes=0-", &eleven, 0, 11, false},
		{"suffix", "bytes=-5", &eleven, 6, 5, false},
		{"suffix-whole", "bytes=-11", &eleven, 0, 11, false},
		{"suffix-oversize", "bytes=-999", &eleven, 0, 11, false},
		{"clamp-end", "bytes=6-999", &eleven, 6, 5, false},
		{"clamp-past-end", "bytes=0-999", &eleven, 0, 11, false},
		{"whitespace", "bytes= 6 - 10 ", &eleven, 6, 5, false},
		{"upper-unit", "BYTES=6-10", &eleven, 6, 5, false},
		{"padded-unit", " bytes =6-10", &eleven, 6, 5, false},
		{"single-byte", "bytes=6-6", &eleven, 6, 1, false},
		{"last-byte", "bytes=10-10", &eleven, 10, 1, false},
		{"empty-file-closed", "bytes=0-4", &zero, 0, 0, true},
		{"empty-file-open", "bytes=0-", &zero, 0, 0, true},
		{"empty-file-suffix", "bytes=-5", &zero, 0, 0, false},
		{"unknown-size", "bytes=0-4", nil, 0, 0, true},
		{"negative-size", "bytes=0-4", model.Ptr(int64(-3)), 0, 0, true},
		{"multi-range", "bytes=0-4,6-10", &eleven, 0, 0, true},
		{"wrong-unit", "items=0-4", &eleven, 0, 0, true},
		{"missing-equals", "bytes", &eleven, 0, 0, true},
		{"missing-dash", "bytes=6", &eleven, 0, 0, true},
		{"start-beyond", "bytes=11-", &eleven, 0, 0, true},
		{"start-past-end", "bytes=12-20", &eleven, 0, 0, true},
		{"reversed", "bytes=8-5", &eleven, 0, 0, true},
		{"suffix-zero", "bytes=-0", &eleven, 0, 0, true},
		{"suffix-negative-text", "bytes=-5-10", &eleven, 0, 0, true},
		// The leading minus is the separator, so a "negative" huge suffix
		// parses as a huge positive one and clamps to the full range.
		{"huge-negative-suffix", "bytes=-99999999999999999999999", &eleven, 0, 11, false},
		{"letters", "bytes=a-b", &eleven, 0, 0, true},
		{"empty-spec", "bytes=", &eleven, 0, 0, true},
		{"huge-start", "bytes=99999999999999999999999-1000", &eleven, 0, 0, true},
		{"huge-end-clamps", "bytes=6-99999999999999999999999", &eleven, 6, 5, false},
		{"huge-suffix-clamps", "bytes=-99999999999999999999999", &eleven, 0, 11, false},
		{"plus-start", "bytes=+6-10", &eleven, 6, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, length, err := parseRangeHeader(tc.value, tc.size)
			if tc.fail {
				if err == nil {
					t.Fatalf("parseRangeHeader(%q) = (%d, %d), want rejection", tc.value, start, length)
				}
				var unsatisfiable *rangeNotSatisfiable
				if !asRangeNotSatisfiable(err, &unsatisfiable) {
					t.Fatalf("parseRangeHeader(%q) error type = %T", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRangeHeader(%q) = %v", tc.value, err)
			}
			if start != tc.start || length != tc.length {
				t.Errorf("parseRangeHeader(%q) = (%d, %d), want (%d, %d)", tc.value, start, length, tc.start, tc.length)
			}
		})
	}
}

func asRangeNotSatisfiable(err error, target **rangeNotSatisfiable) bool {
	if typed, ok := err.(*rangeNotSatisfiable); ok {
		*target = typed
		return true
	}
	return false
}

// TestIfRangeMatches pins the current-ETag-only semantics: quoted and bare
// spellings match after trimming, everything else — dates included — never
// matches, and a missing header always does.
func TestIfRangeMatches(t *testing.T) {
	entry := model.RemoteEntry{Etag: model.Ptr("abc123")}
	bare := model.RemoteEntry{Etag: model.Ptr(`"quoted-1"`)}
	empty := model.RemoteEntry{Etag: model.Ptr("")}
	absent := model.RemoteEntry{}

	cases := []struct {
		name    string
		ifRange string
		entry   model.RemoteEntry
		want    bool
	}{
		{"absent-header", "", entry, true},
		{"quoted-match", `"abc123"`, entry, true},
		{"bare-match", "abc123", entry, true},
		{"padded-match", `  "abc123"  `, entry, true},
		{"mismatch", `"other"`, entry, false},
		{"date-form", "Tue, 01 Sep 2026 13:11:12 GMT", entry, false},
		{"weak-etags", `W/"abc123"`, entry, false},
		{"blank-header", "   ", entry, false},
		{"empty-etag", `"abc123"`, empty, false},
		{"absent-etag", `"abc123"`, absent, false},
		{"quoted-entry-bare-value", "quoted-1", bare, true},
		{"quoted-entry-quoted-value", `"quoted-1"`, bare, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ifRangeMatches(tc.ifRange, tc.entry); got != tc.want {
				t.Errorf("ifRangeMatches(%q) = %v, want %v", tc.ifRange, got, tc.want)
			}
		})
	}
}

// rangeFixture mirrors tests/test_server.py's RangeStorage: hello.txt is
// eleven bytes with etag abc123, and the range-aware fake slices the
// payload by the requested offset/length.
func rangeFixture(t *testing.T, size *int64, etag string) *downloadStorageFake {
	t.Helper()
	entry := model.RemoteEntry{
		ID:   "file-1",
		Name: "hello.txt",
		Kind: model.KindFile,
		Size: size,
		Etag: model.Ptr(etag),
	}
	return &downloadStorageFake{entry: entry, payload: "hello world"}
}

// rangeRouter wires the range fixture into both surfaces: DAV metadata and
// the range-aware download storage.
func rangeRouter(t *testing.T, storage *downloadStorageFake) *Router {
	t.Helper()
	return newDownloadRouterWithDAV(t, &davHeadStorage{entry: storage.entry}, storage, DownloadLimits{StreamChunkSize: 4})
}

// TestDAVGetRangeGoldens replays the wire behavior captured from the live
// Python reference server for closed, open, and suffix ranges plus the 416
// framing.
func TestDAVGetRangeGoldens(t *testing.T) {
	eleven := int64(11)
	cases := []struct {
		name        string
		headers     []string
		status      int
		start       int64
		count       int64
		contentType string
	}{
		{"closed", []string{"Range: bytes=6-10"}, 206, 6, 5, "text/plain"},
		{"open", []string{"Range: bytes=6-"}, 206, 6, 5, "text/plain"},
		{"suffix", []string{"Range: bytes=-5"}, 206, 6, 5, "text/plain"},
		{"clamp", []string{"Range: bytes=6-999"}, 206, 6, 5, "text/plain"},
		{"ifrange-quoted", []string{"Range: bytes=6-10", `If-Range: "abc123"`}, 206, 6, 5, "text/plain"},
		{"ifrange-bare", []string{"Range: bytes=6-10", "If-Range: abc123"}, 206, 6, 5, "text/plain"},
		{"ifrange-mismatch", []string{"Range: bytes=6-10", `If-Range: "other"`}, 200, 0, 11, "text/plain"},
		{"ifrange-date", []string{"Range: bytes=6-10", "If-Range: Tue, 01 Sep 2026 13:11:12 GMT"}, 200, 0, 11, "text/plain"},
		{"ifrange-unknown-etag", []string{"Range: bytes=6-10", `If-Range: "abc123"`}, 206, 6, 5, "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size := &eleven
			etag := "abc123"
			if tc.name == "ifrange-unknown-etag" {
				// An octet-stream entry (no extension) keeps the flow but
				// proves the content type comes from the entry name.
				entry := model.RemoteEntry{ID: "file-9", Name: "blob", Kind: model.KindFile, Size: size, Etag: model.Ptr(etag)}
				storage := &downloadStorageFake{entry: entry, payload: "hello world"}
				recorder := httptest.NewRecorder()
				request := newTestRequest("GET", "/dav/blob")
				for _, header := range tc.headers {
					name, value, _ := strings.Cut(header, ": ")
					request.Header.Set(name, value)
				}
				rangeRouter(t, storage).ServeHTTP(recorder, request)
				assertRangeResponse(t, recorder, tc.status, tc.start, tc.count, tc.contentType, "hello world"[tc.start:tc.start+tc.count], etag)
				return
			}
			storage := rangeFixture(t, size, etag)
			recorder := httptest.NewRecorder()
			request := newTestRequest("GET", "/dav/hello.txt")
			for _, header := range tc.headers {
				name, value, _ := strings.Cut(header, ": ")
				request.Header.Set(name, value)
			}
			rangeRouter(t, storage).ServeHTTP(recorder, request)
			assertRangeResponse(t, recorder, tc.status, tc.start, tc.count, tc.contentType, "hello world"[tc.start:tc.start+tc.count], etag)
		})
	}
}

func assertRangeResponse(t *testing.T, recorder *httptest.ResponseRecorder, status int, start int64, count int64, contentType string, body string, etag string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d", recorder.Code, status)
	}
	header := recorder.Header()
	if got := header.Get("Content-Type"); got != contentType {
		t.Errorf("Content-Type = %q", got)
	}
	if etagValue := header["ETag"]; len(etagValue) != 1 || etagValue[0] != `"`+etag+`"` {
		t.Errorf("ETag = %q", etagValue)
	}
	if status == http.StatusPartialContent {
		if got := header.Get("Content-Range"); got != fmt.Sprintf("bytes %d-%d/11", start, start+count-1) {
			t.Errorf("Content-Range = %q", got)
		}
		if got := header.Get("Content-Length"); got != fmt.Sprintf("%d", count) {
			t.Errorf("Content-Length = %q", got)
		}
	} else if got := header.Get("Content-Length"); got != "11" {
		t.Errorf("Content-Length = %q", got)
	}
	if recorder.Body.String() != body {
		t.Errorf("body = %q, want %q", recorder.Body.String(), body)
	}
}

// TestDAVGetRangeUnsatisfiable pins the 416 framing: text body, the
// "bytes */N" Content-Range, no ETag, and no Connection: close.
func TestDAVGetRangeUnsatisfiable(t *testing.T) {
	cases := []struct {
		name         string
		rangeHeader  string
		size         *int64
		contentRange string
	}{
		{"start-beyond", "bytes=20-30", model.Ptr(int64(11)), "bytes */11"},
		{"multi", "bytes=0-4,6-10", model.Ptr(int64(11)), "bytes */11"},
		{"wrong-unit", "items=0-4", model.Ptr(int64(11)), "bytes */11"},
		{"reversed", "bytes=8-5", model.Ptr(int64(11)), "bytes */11"},
		{"empty-file", "bytes=0-4", model.Ptr(int64(0)), "bytes */0"},
		{"unknown-size", "bytes=0-4", nil, "bytes */*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storage := rangeFixture(t, tc.size, "abc123")
			recorder := httptest.NewRecorder()
			request := newTestRequest("GET", "/dav/hello.txt")
			request.Header.Set("Range", tc.rangeHeader)
			rangeRouter(t, storage).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusRequestedRangeNotSatisfiable {
				t.Fatalf("status = %d", recorder.Code)
			}
			if got := recorder.Body.String(); got != "requested byte range cannot be satisfied\n" {
				t.Errorf("body = %q", got)
			}
			if got := recorder.Header().Get("Content-Range"); got != tc.contentRange {
				t.Errorf("Content-Range = %q", got)
			}
			if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
			if got := recorder.Header().Get("Connection"); got != "" {
				t.Errorf("416 unexpectedly closes: %q", got)
			}
			if _, present := recorder.Header()["ETag"]; present {
				t.Errorf("416 must not carry the ETag")
			}
			if len(storage.opened) != 0 {
				t.Errorf("416 acquired the download slot: %v", storage.opened)
			}
		})
	}
}

// TestDAVHeadRangeGolden pins the captured Python HEAD+Range wire: 206 with
// the range headers and no body, while a mismatched If-Range reports the
// full entry.
func TestDAVHeadRangeGolden(t *testing.T) {
	eleven := int64(11)
	storage := rangeFixture(t, &eleven, "abc123")
	router := rangeRouter(t, storage)

	recorder := httptest.NewRecorder()
	request := newTestRequest("HEAD", "/dav/hello.txt")
	request.Header.Set("Range", "bytes=6-10")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	header := recorder.Header()
	if got := header.Get("Content-Range"); got != "bytes 6-10/11" {
		t.Errorf("Content-Range = %q", got)
	}
	if got := header.Get("Content-Length"); got != "5" {
		t.Errorf("Content-Length = %q", got)
	}
	if got := header.Get("Connection"); got != "close" {
		t.Errorf("Connection = %q", got)
	}
	if got := header["ETag"]; len(got) != 1 || got[0] != `"abc123"` {
		t.Errorf("ETag = %q", got)
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("HEAD body = %q", recorder.Body.String())
	}
	if len(storage.opened) != 0 {
		t.Errorf("HEAD+Range opened a stream: %v", storage.opened)
	}

	recorder = httptest.NewRecorder()
	request = newTestRequest("HEAD", "/dav/hello.txt")
	request.Header.Set("Range", "bytes=6-10")
	request.Header.Set("If-Range", `"other"`)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Length") != "11" {
		t.Errorf("mismatched If-Range = %d / CL %q", recorder.Code, recorder.Header().Get("Content-Length"))
	}

	recorder = httptest.NewRecorder()
	request = newTestRequest("HEAD", "/dav/hello.txt")
	request.Header.Set("Range", "bytes=20-30")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("416 status = %d", recorder.Code)
	}
}

// TestDAVGetRangeUpstreamLengthMismatch pins the boundary check: a range
// request whose stream delivers a different length fails before any byte
// is written, closing the stream and releasing the slot.
func TestDAVGetRangeUpstreamLengthMismatch(t *testing.T) {
	eleven := int64(11)
	entry := model.RemoteEntry{ID: "file-1", Name: "hello.txt", Kind: model.KindFile, Size: &eleven, Etag: model.Ptr("abc123")}
	stream := newFakeStream("world", model.Ptr(int64(4)))
	storage := &downloadStorageFake{entry: entry, stream: stream}
	recorder := httptest.NewRecorder()
	request := newTestRequest("GET", "/dav/hello.txt")
	request.Header.Set("Range", "bytes=6-10")
	rangeRouter(t, storage).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != "upstream WPS request failed\n" {
		t.Errorf("body = %q", body)
	}
	if stream.closeCalls() != 1 {
		t.Errorf("close calls = %d", stream.closeCalls())
	}
}

// TestDAVGetEmptyFileSuffixRange pins the captured zero-size quirk: the
// suffix form yields the (0, 0) parse with the "bytes 0--1/0" header
// framing and an empty 206 body.
func TestDAVGetEmptyFileSuffixRange(t *testing.T) {
	zero := int64(0)
	entry := model.RemoteEntry{ID: "file-2", Name: "empty.bin", Kind: model.KindFile, Size: &zero}
	stream := newFakeStream("", &zero)
	storage := &downloadStorageFake{entry: entry, stream: stream}
	recorder := httptest.NewRecorder()
	request := newTestRequest("GET", "/dav/empty.bin")
	request.Header.Set("Range", "bytes=-5")
	rangeRouter(t, storage).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 0--1/0" {
		t.Errorf("Content-Range = %q", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != "0" {
		t.Errorf("Content-Length = %q", got)
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("body = %q", recorder.Body.String())
	}
}

// TestRESTDownloadRange pins the JSON framing of the 416 on the REST
// download route.
func TestRESTDownloadRange(t *testing.T) {
	eleven := int64(11)
	storage := rangeFixture(t, &eleven, "abc123")
	router := newDownloadRouter(t, storage, DownloadLimits{})

	recorder := httptest.NewRecorder()
	request := newTestRequest("GET", "/api/v1/download?path=%2Fhello.txt")
	request.Header.Set("Range", "bytes=6-10")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 6-10/11" {
		t.Errorf("Content-Range = %q", got)
	}
	if got := recorder.Header().Get("Content-Disposition"); got == "" {
		t.Errorf("REST range download lost Content-Disposition")
	}

	recorder = httptest.NewRecorder()
	request = newTestRequest("GET", "/api/v1/download?path=%2Fhello.txt")
	request.Header.Set("Range", "bytes=20-30")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("416 status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"error":"requested byte range cannot be satisfied"}` {
		t.Errorf("body = %q", body)
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes */11" {
		t.Errorf("Content-Range = %q", got)
	}
}
