// The download tests mirror _send_download's GET branch: exact header
// surfaces, byte-exact chunked streaming, close framing for unknown
// lengths, the slot and body released on every exit, and the DAV/REST
// contract goldens.

package httpserver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// stubDownloadStorage satisfies the download surface for routes a test
// never exercises.
type stubDownloadStorage struct{}

func (stubDownloadStorage) Metadata(string) (model.RemoteEntry, error) {
	return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "not found")
}

func (stubDownloadStorage) OpenPath(context.Context, string, int64, *int64) (DownloadStream, error) {
	return nil, errors.New("download is not wired in this test")
}

// fakeDownloadStream scripts one upstream object response and records the
// requested chunk sizes and closes.
type fakeDownloadStream struct {
	data          io.Reader
	contentType   *string
	contentLength *int64
	contentRange  *string

	mu         sync.Mutex
	readSizes  []int
	closeCount int
	readErrs   []error
	closeOnce  sync.Once
}

func newFakeStream(data string, contentLength *int64) *fakeDownloadStream {
	return &fakeDownloadStream{data: strings.NewReader(data), contentLength: contentLength}
}

func (s *fakeDownloadStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	s.readSizes = append(s.readSizes, len(p))
	errs := s.readErrs
	s.mu.Unlock()
	if len(errs) > 0 {
		return 0, errs[0]
	}
	return s.data.Read(p)
}

func (s *fakeDownloadStream) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closeCount++
		s.mu.Unlock()
	})
	return nil
}

func (s *fakeDownloadStream) closeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCount
}

func (s *fakeDownloadStream) sizes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.readSizes...)
}

func (s *fakeDownloadStream) HTTPStatus() int       { return 200 }
func (s *fakeDownloadStream) ContentType() *string  { return s.contentType }
func (s *fakeDownloadStream) ContentLength() *int64 { return s.contentLength }
func (s *fakeDownloadStream) ContentRange() *string { return s.contentRange }

// downloadStorageFake is the storage double: metadata answers from byPath,
// open_path hands out the scripted streams and records the openings.
type downloadStorageFake struct {
	entry   model.RemoteEntry
	byPath  map[string]model.RemoteEntry
	metaErr error
	openErr error
	stream  DownloadStream
	streams []DownloadStream
	opened  []string
	offsets []int64
	// payload turns OpenPath into a range-aware object store: the data is
	// sliced by the requested offset/length and the stream length follows
	// the slice, mirroring tests/test_server.py's RangeStorage.
	payload string
}

func (f *downloadStorageFake) Metadata(path string) (model.RemoteEntry, error) {
	if f.metaErr != nil {
		return model.RemoteEntry{}, f.metaErr
	}
	if f.byPath != nil {
		if entry, ok := f.byPath[path]; ok {
			return entry, nil
		}
	}
	return f.entry, nil
}

func (f *downloadStorageFake) OpenPath(_ context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	f.opened = append(f.opened, path)
	f.offsets = append(f.offsets, offset)
	if f.openErr != nil {
		return nil, f.openErr
	}
	if f.payload != "" {
		data := f.payload[offset:]
		if length != nil {
			data = data[:*length]
		}
		return newFakeStream(data, model.Ptr(int64(len(data)))), nil
	}
	if len(f.streams) > 0 {
		next := f.streams[0]
		f.streams = f.streams[1:]
		return next, nil
	}
	return f.stream, nil
}

func downloadFileEntry() model.RemoteEntry {
	size := int64(11)
	return model.RemoteEntry{
		ID:   "bench-file-1",
		Name: "bench-one.txt",
		Kind: model.KindFile,
		Size: &size,
		Etag: model.Ptr("bench-etag-bench-file-1"),
	}
}

const downloadPayload = "bench-bytes"

func downloadStorage(t *testing.T, stream DownloadStream) *downloadStorageFake {
	t.Helper()
	return &downloadStorageFake{entry: downloadFileEntry(), stream: stream}
}

func newDownloadRouter(t *testing.T, downloads DownloadStorage, limits DownloadLimits) *Router {
	t.Helper()
	return newDownloadRouterWithDAV(t, &davHeadStorage{entry: downloadFileEntry()}, downloads, limits)
}

func newDownloadRouterWithDAV(t *testing.T, storage DAVStorage, downloads DownloadStorage, limits DownloadLimits) *Router {
	t.Helper()
	dispatcher, err := NewDAVDispatcher(storage, ControlLimits{}, DAVLimits{}, limits, downloads, stubUploadStorage{}, stubMutations{}, newTestLockStore(t), 0, "/dav")
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(RouterConfig{
		DAVPrefix:  "/dav",
		RESTPrefix: "/api/v1",
		Handlers: Handlers{
			Health:   func(w http.ResponseWriter, r *http.Request) {},
			WebApp:   func(w http.ResponseWriter, r *http.Request) {},
			WebAsset: func(w http.ResponseWriter, r *http.Request, name string) {},
			REST: func(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
				path, err := queryPath(route.Query)
				if err != nil {
					return err
				}
				if route.Suffix == "preview" {
					return sendPreview(w, r, path, downloads, limits)
				}
				return sendDownload(w, r, path, true, downloads, limits.chunkSize())
			},
			DAV: dispatcher.ServeDAV,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return router
}

// TestDAVDownloadFileStreamsExactly pins the full GET header set and the
// chunked copy: the body arrives byte-exact, reads respect the configured
// chunk size with the remaining+1 cap, and the stream closes exactly once.
func TestDAVDownloadFileStreamsExactly(t *testing.T) {
	stream := newFakeStream(downloadPayload, model.Ptr(int64(11)))
	storage := downloadStorage(t, stream)
	router := newDownloadRouter(t, storage, DownloadLimits{StreamChunkSize: 4})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/dav/bench-one.txt"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	digest := sha256.Sum256([]byte(downloadPayload))
	if body := recorder.Body.String(); body != downloadPayload {
		t.Errorf("body = %q, want %q", body, downloadPayload)
	}
	if hashed := hex.EncodeToString(digest[:]); hashed !=
		"ca809146f650802ba8c8a4e1077e4bbb24745af5e521c3837128515fe59e1fb2" {
		t.Errorf("sha256 = %s", hashed)
	}
	header := recorder.Header()
	want := map[string]string{
		"Content-Type":           "text/plain",
		"Accept-Ranges":          "bytes",
		"Cache-Control":          "no-store, no-transform",
		"X-Content-Type-Options": "nosniff",
		"Content-Length":         "11",
		"Connection":             "close",
	}
	for name, value := range want {
		if got := header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	// The handler writes the raw "ETag" key; Get would look up the
	// canonicalized "Etag" and miss it.
	if etag := header["ETag"]; len(etag) != 1 || etag[0] != `"bench-etag-bench-file-1"` {
		t.Errorf("ETag = %q", etag)
	}
	if _, present := header["Content-Disposition"]; present {
		t.Errorf("DAV download must not carry Content-Disposition")
	}
	if len(storage.opened) != 1 || storage.opened[0] != "/bench-one.txt" || storage.offsets[0] != 0 {
		t.Errorf("open calls = %v offsets %v", storage.opened, storage.offsets)
	}
	// remaining+1 capping: reads of 4, then 4, then a final capped read of
	// 4 that the stream answers with the last 3 bytes.
	if sizes := stream.sizes(); len(sizes) != 3 || sizes[0] != 4 || sizes[1] != 4 || sizes[2] != 4 {
		t.Errorf("read sizes = %v", stream.sizes())
	}
	if stream.closeCalls() != 1 {
		t.Errorf("close calls = %d", stream.closeCalls())
	}
}

// TestDAVDownloadUnknownLengthUsesCloseFraming pins the unknown-length
// contract on the wire: no Content-Length, no Transfer-Encoding, the
// duplicated Python Connection header, and an EOF-terminated body.
func TestDAVDownloadUnknownLengthUsesCloseFraming(t *testing.T) {
	stream := newFakeStream("chunked-out-payload", nil)
	storage := downloadStorage(t, stream)
	server := httptest.NewServer(newDownloadRouter(t, storage, DownloadLimits{}))
	defer server.Close()

	response, body, err := rawDownloadRequest(t, server.Listener.Addr().String(), "GET /dav/bench-one.txt HTTP/1.1\r\nHost: x\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	text := string(response)
	if !strings.HasPrefix(text, "HTTP/1.1 200 OK\r\n") {
		t.Errorf("status line = %q", text[:strings.Index(text, "\r\n")])
	}
	if strings.Contains(text, "Transfer-Encoding:") {
		t.Errorf("identity transfer encoding leaked onto the wire:\n%s", text)
	}
	if strings.Contains(text, "Content-Length:") {
		t.Errorf("unknown length must not advertise Content-Length:\n%s", text)
	}
	if count := strings.Count(text, "Connection: close"); count != 2 {
		t.Errorf("Connection: close count = %d, want the Python duplicate pair:\n%s", count, text)
	}
	if !strings.HasSuffix(text, "chunked-out-payload") {
		t.Errorf("body missing: %q", text)
	}
	if string(body) != "chunked-out-payload" {
		t.Errorf("body = %q", body)
	}
	if stream.closeCalls() != 1 {
		t.Errorf("close calls = %d", stream.closeCalls())
	}
}

// TestDAVDownloadKnownLengthFramesExactly pins the known-length wire: one
// Connection header, exact Content-Length, EOF right after the body.
func TestDAVDownloadKnownLengthFramesExactly(t *testing.T) {
	stream := newFakeStream(downloadPayload, model.Ptr(int64(11)))
	storage := downloadStorage(t, stream)
	server := httptest.NewServer(newDownloadRouter(t, storage, DownloadLimits{}))
	defer server.Close()

	response, body, err := rawDownloadRequest(t, server.Listener.Addr().String(), "GET /dav/bench-one.txt HTTP/1.1\r\nHost: x\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	text := string(response)
	if !strings.Contains(text, "Content-Length: 11\r\n") {
		t.Errorf("missing Content-Length:\n%s", text)
	}
	if count := strings.Count(text, "Connection: close"); count != 1 {
		t.Errorf("Connection: close count = %d, want one:\n%s", count, text)
	}
	if strings.Contains(text, "Transfer-Encoding:") {
		t.Errorf("unexpected transfer encoding:\n%s", text)
	}
	if string(body) != downloadPayload {
		t.Errorf("body = %q", body)
	}
}

// TestDAVDownloadShortReadEndsConnection covers the ended-early warning
// path: the declared length is advertised, the stream dies sooner, the
// client sees the short body and then EOF, and the slot is released.
func TestDAVDownloadShortReadEndsConnection(t *testing.T) {
	stream := newFakeStream("short", model.Ptr(int64(10)))
	storage := downloadStorage(t, stream)
	server := httptest.NewServer(newDownloadRouter(t, storage, DownloadLimits{}))
	defer server.Close()

	response, body, err := rawDownloadRequest(t, server.Listener.Addr().String(), "GET /dav/bench-one.txt HTTP/1.1\r\nHost: x\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), "Content-Length: 10\r\n") {
		t.Errorf("declared length missing:\n%s", response)
	}
	if string(body) != "short" {
		t.Errorf("body = %q, want the five delivered bytes", body)
	}
	if stream.closeCalls() != 1 {
		t.Errorf("close calls = %d", stream.closeCalls())
	}
}

// TestDAVDownloadOverLongStreamTruncates pins the exceeded-length path: the
// stream sends more than declared, the client receives exactly the promised
// bytes and then EOF.
func TestDAVDownloadOverLongStreamTruncates(t *testing.T) {
	stream := newFakeStream("0123456789-extra", model.Ptr(int64(5)))
	storage := downloadStorage(t, stream)
	server := httptest.NewServer(newDownloadRouter(t, storage, DownloadLimits{}))
	defer server.Close()

	response, body, err := rawDownloadRequest(t, server.Listener.Addr().String(), "GET /dav/bench-one.txt HTTP/1.1\r\nHost: x\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), "Content-Length: 5\r\n") {
		t.Errorf("declared length missing:\n%s", response)
	}
	if string(body) != "01234" {
		t.Errorf("body = %q, want the truncated promise", body)
	}
	if stream.closeCalls() != 1 {
		t.Errorf("close calls = %d", stream.closeCalls())
	}
}

// TestDAVDownloadDisconnectReleasesSlot covers the mid-download client
// abort: the loop stops at the next disconnect check and the stream closes
// exactly once.
func TestDAVDownloadDisconnectReleasesSlot(t *testing.T) {
	reader, writer := io.Pipe()
	stream := &fakeDownloadStream{data: reader}
	storage := downloadStorage(t, stream)
	server := httptest.NewServer(newDownloadRouter(t, storage, DownloadLimits{}))
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(conn, "GET /dav/bench-one.txt HTTP/1.1\r\nHost: x\r\n\r\n")
	// The upstream produces one chunk, then holds.
	if _, err := writer.Write([]byte("first-chunk-and-more")); err != nil {
		t.Fatal(err)
	}
	buffered := bufio.NewReader(conn)
	if _, err := http.ReadResponse(buffered, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := buffered.Peek(20); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	writer.Close()

	deadline := time.Now().Add(2 * time.Second)
	for stream.closeCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if stream.closeCalls() != 1 {
		t.Fatalf("stream was never closed after the disconnect")
	}
}

// TestDAVDownloadErrors pins the pre-response error surfaces: folder paths
// conflict, missing paths 404, and neither opens a stream.
func TestDAVDownloadErrors(t *testing.T) {
	folder := downloadFileEntry()
	folder.Kind = model.KindFolder
	storage := &downloadStorageFake{byPath: map[string]model.RemoteEntry{"/": folder}}
	router := newDownloadRouter(t, storage, DownloadLimits{})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/dav/"))
	if recorder.Code != http.StatusConflict {
		t.Errorf("folder status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != "the requested path is not a file\n" {
		t.Errorf("folder body = %q", body)
	}
	if len(storage.opened) != 0 {
		t.Errorf("folder path opened a stream: %v", storage.opened)
	}

	missing := &downloadStorageFake{metaErr: model.NewStorageError(model.KindEntryNotFound, "no such path: /missing.txt")}
	router = newDownloadRouter(t, missing, DownloadLimits{})
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/dav/missing.txt"))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("missing status = %d", recorder.Code)
	}

	busy := downloadStorage(t, nil)
	busy.openErr = model.NewStorageError(model.KindServiceBusy, "too many downloads are active")
	router = newDownloadRouter(t, busy, DownloadLimits{})
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/dav/bench-one.txt"))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("busy status = %d", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") != "5" {
		t.Errorf("Retry-After = %q", recorder.Header().Get("Retry-After"))
	}
}

// TestRESTDownloadStreamsObjectBytes mirrors REST-DOWNLOAD-001: the REST
// route adds the Content-Disposition pair and keeps the body identical.
func TestRESTDownloadStreamsObjectBytes(t *testing.T) {
	stream := newFakeStream(downloadPayload, model.Ptr(int64(11)))
	storage := downloadStorage(t, stream)
	router := newDownloadRouter(t, storage, DownloadLimits{})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/download?path=%2Fbench-one.txt"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	header := recorder.Header()
	if got := header.Get("Content-Disposition"); got !=
		`attachment; filename="download.txt"; filename*=UTF-8''bench-one.txt` {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := header.Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := header.Get("Content-Length"); got != "11" {
		t.Errorf("Content-Length = %q", got)
	}
	if body := recorder.Body.String(); body != downloadPayload {
		t.Errorf("body = %q", body)
	}
	digest := sha256.Sum256(recorder.Body.Bytes())
	if hashed := hex.EncodeToString(digest[:]); hashed !=
		"ca809146f650802ba8c8a4e1077e4bbb24745af5e521c3837128515fe59e1fb2" {
		t.Errorf("sha256 = %s", hashed)
	}
}

// TestRESTDownloadErrorFraming pins the JSON error shape for the REST
// download route.
func TestRESTDownloadErrorFraming(t *testing.T) {
	folder := downloadFileEntry()
	folder.Kind = model.KindFolder
	storage := &downloadStorageFake{byPath: map[string]model.RemoteEntry{"/": folder}}
	router := newDownloadRouter(t, storage, DownloadLimits{})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/download"))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"error":"the requested path is not a file"}` {
		t.Errorf("body = %q", body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
}

// TestAsciiDownloadName pins _ascii_download_name table-style.
func TestAsciiDownloadName(t *testing.T) {
	cases := []struct{ name, want string }{
		{"bench-one.txt", "download.txt"},
		{"noext", "download"},
		{"trailing.", "download"},
		{".hidden", "download.hidden"},
		{"a.b@c", "download.bc"},
		{"UPPER.TAR", "download.TAR"},
		{"x." + strings.Repeat("a", 40), "download." + strings.Repeat("a", 32)},
		{"dots..txt", "download.txt"},
	}
	for _, tc := range cases {
		if got := asciiDownloadName(tc.name); got != tc.want {
			t.Errorf("asciiDownloadName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestDownloadLimitsDefaults pins the stream_chunk_size default.
func TestDownloadLimitsDefaults(t *testing.T) {
	if got := (DownloadLimits{}).chunkSize(); got != 1<<20 {
		t.Errorf("default chunk size = %d", got)
	}
	if got := (DownloadLimits{StreamChunkSize: -1}).chunkSize(); got != 1<<20 {
		t.Errorf("negative chunk size = %d", got)
	}
	if got := (DownloadLimits{StreamChunkSize: 7}).chunkSize(); got != 7 {
		t.Errorf("configured chunk size = %d", got)
	}
	if got := (DownloadLimits{}).previewLimit(); got != 2*1024*1024 {
		t.Errorf("default preview limit = %d", got)
	}
	if got := (DownloadLimits{PreviewMaxBytes: 7}).previewLimit(); got != 7 {
		t.Errorf("configured preview limit = %d", got)
	}
}

func TestRESTTextPreviewStreamsWithoutDownloadDisposition(t *testing.T) {
	stream := newFakeStream(downloadPayload, model.Ptr(int64(len(downloadPayload))))
	storage := downloadStorage(t, stream)
	router := newDownloadRouter(t, storage, DownloadLimits{})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/preview?path=%2Fbench-one.txt"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != downloadPayload {
		t.Errorf("body = %q, want %q", recorder.Body.String(), downloadPayload)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("Content-Disposition = %q, want absent", got)
	}
	if got := recorder.Header().Get("X-Preview-Truncated"); got != "false" {
		t.Errorf("X-Preview-Truncated = %q", got)
	}
	if stream.closeCalls() != 1 {
		t.Errorf("stream close calls = %d, want 1", stream.closeCalls())
	}
}

func TestRESTTextPreviewIsBounded(t *testing.T) {
	storage := downloadStorage(t, nil)
	storage.payload = "abcdef"
	router := newDownloadRouter(t, storage, DownloadLimits{PreviewMaxBytes: 4})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/preview?path=%2Fbench-one.txt"))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "abcd" {
		t.Fatalf("status/body = %d %q, want 200 %q", recorder.Code, recorder.Body.String(), "abcd")
	}
	if got := recorder.Header().Get("X-Preview-Truncated"); got != "true" {
		t.Errorf("X-Preview-Truncated = %q", got)
	}
}

func TestRESTTextPreviewRejectsNonTextFiles(t *testing.T) {
	entry := downloadFileEntry()
	entry.Name = "bench-one.bin"
	storage := &downloadStorageFake{entry: entry, stream: newFakeStream(downloadPayload, model.Ptr(int64(len(downloadPayload))))}
	router := newDownloadRouter(t, storage, DownloadLimits{})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/preview?path=%2Fbench-one.bin"))
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != `{"error":"only .txt files can be previewed"}` {
		t.Errorf("body = %q", recorder.Body.String())
	}
}

// TestDownloadContractGoldens replays the recorded DAV-GET and
// REST-DOWNLOAD evidence against the Go dispatcher.
func TestDownloadContractGoldens(t *testing.T) {
	dir := filepath.Clean(filepath.Join("../../../contract_tests", "results"))
	record := func(name string) map[string]any {
		raw, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}

	t.Run("DAV-GET-001 file", func(t *testing.T) {
		want := record("DAV-GET-001")
		stream := newFakeStream(want["body"].(string), model.Ptr(int64(11)))
		router := newDownloadRouter(t, downloadStorage(t, stream), DownloadLimits{})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/dav/bench-one.txt"))
		if recorder.Code != int(want["status"].(float64)) {
			t.Fatalf("status = %d", recorder.Code)
		}
		for name, key := range map[string]string{
			"Content-Type":   "content_type",
			"Content-Length": "content_length",
			"Accept-Ranges":  "accept_ranges",
			"Cache-Control":  "cache_control",
			"Connection":     "connection",
		} {
			if got := recorder.Header().Get(name); got != want[key].(string) {
				t.Errorf("%s = %q, want %v", name, got, want[key])
			}
		}
		// The raw "ETag" key never surfaces through Get.
		if etag := recorder.Header()["ETag"]; len(etag) != 1 || etag[0] != want["etag"].(string) {
			t.Errorf("ETag = %q, want %v", etag, want["etag"])
		}
		if recorder.Body.String() != want["body"].(string) {
			t.Errorf("body = %q", recorder.Body.String())
		}
	})

	t.Run("DAV-GET-002 folder conflicts", func(t *testing.T) {
		want := record("DAV-GET-002")
		folder := downloadFileEntry()
		folder.Kind = model.KindFolder
		storage := &downloadStorageFake{byPath: map[string]model.RemoteEntry{"/": folder}}
		router := newDownloadRouter(t, storage, DownloadLimits{})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/dav/"))
		if recorder.Code != int(want["status"].(float64)) {
			t.Errorf("status = %d", recorder.Code)
		}
		if recorder.Body.String() != want["body"].(string) {
			t.Errorf("body = %q, want %q", recorder.Body.String(), want["body"])
		}
	})

	t.Run("DAV-GET-003 missing 404", func(t *testing.T) {
		want := record("DAV-GET-003")
		storage := &downloadStorageFake{metaErr: model.NewStorageError(model.KindEntryNotFound, "no such path")}
		router := newDownloadRouter(t, storage, DownloadLimits{})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/dav/missing.txt"))
		if recorder.Code != int(want["status"].(float64)) {
			t.Errorf("status = %d", recorder.Code)
		}
	})

	t.Run("REST-DOWNLOAD-001 streams object bytes", func(t *testing.T) {
		want := record("REST-DOWNLOAD-001")
		stream := newFakeStream(want["body"].(string), model.Ptr(int64(11)))
		router := newDownloadRouter(t, downloadStorage(t, stream), DownloadLimits{})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/download?path=%2Fbench-one.txt"))
		if recorder.Code != int(want["status"].(float64)) {
			t.Fatalf("status = %d", recorder.Code)
		}
		if got := recorder.Header().Get("Content-Disposition"); got != want["content_disposition"].(string) {
			t.Errorf("Content-Disposition = %q, want %v", got, want["content_disposition"])
		}
		if got := recorder.Header().Get("Content-Type"); got != want["content_type"].(string) {
			t.Errorf("Content-Type = %q", got)
		}
		if got := recorder.Header().Get("Content-Length"); got != want["content_length"].(string) {
			t.Errorf("Content-Length = %q", got)
		}
		digest := sha256.Sum256(recorder.Body.Bytes())
		if hashed := hex.EncodeToString(digest[:]); hashed != want["sha256"].(string) {
			t.Errorf("sha256 = %s, want %v", hashed, want["sha256"])
		}
	})
}

// rawDownloadRequest performs one raw HTTP/1.1 request and returns the full
// wire bytes (headers plus body) and the body alone once the connection
// closes.
func rawDownloadRequest(t *testing.T, address string, request string) ([]byte, []byte, error) {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(request)); err != nil {
		return nil, nil, err
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	payload, err := io.ReadAll(conn)
	if err != nil {
		return nil, nil, err
	}
	split := strings.Index(string(payload), "\r\n\r\n")
	if split < 0 {
		return payload, nil, fmt.Errorf("no header block in %q", payload)
	}
	return payload, payload[split+4:], nil
}
