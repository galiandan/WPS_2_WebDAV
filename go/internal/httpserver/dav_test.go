package httpserver

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// TestGuessMimeTypeMirrorsPythonTable pins the mimetypes.guess_type port:
// case-insensitive lookup, suffix_map rewriting, encoding peels, and the
// octet-stream fallback for unknown or extension-less names.
func TestGuessMimeTypeMirrorsPythonTable(t *testing.T) {
	cases := map[string]string{
		"bench-one.txt": "text/plain",
		"PHOTO.JPG":     "image/jpeg",
		"notes.md":      "text/markdown",
		"icon.ico":      "image/vnd.microsoft.icon",
		"clip.wav":      "audio/x-wav",
		"movie.mp4":     "video/mp4",
		"page.html":     "text/html",
		"data.json":     "application/json",
		"doc.docx":      "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"x.tar.gz":      "application/x-tar",
		"archive.tgz":   "application/x-tar",
		"backup.tbz2":   "application/x-tar",
		"blob.svgz":     "image/svg+xml",
		"bundle.txz":    "application/x-tar",
		"x.gz":          "application/octet-stream",
		"x.br":          "application/octet-stream",
		"noext":         "application/octet-stream",
		".hidden":       "application/octet-stream",
		"x.":            "application/octet-stream",
		"weird.xyzzy":   "application/octet-stream",
	}
	for name, want := range cases {
		if got := guessMimeType(name); got != want {
			t.Errorf("guessMimeType(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestPythonSplitExt pins the splitext port on the leading-dot rule.
func TestPythonSplitExt(t *testing.T) {
	cases := []struct{ in, base, ext string }{
		{"bench-one.txt", "bench-one", ".txt"},
		{"x.tar.gz", "x.tar", ".gz"},
		{".hidden", ".hidden", ""},
		{"..dots", "..dots", ""},
		{"...a.txt", "...a", ".txt"},
		{".a.b", ".a", ".b"},
		{"x.", "x", "."},
		{"noext", "noext", ""},
		{"/a/b/c.txt", "/a/b/c", ".txt"},
	}
	for _, tc := range cases {
		base, ext := pythonSplitExt(tc.in)
		if base != tc.base || ext != tc.ext {
			t.Errorf("pythonSplitExt(%q) = (%q, %q), want (%q, %q)", tc.in, base, ext, tc.base, tc.ext)
		}
	}
}

// davHeadStorage is a configurable fake for the HEAD routes: entries can
// be selected per path (the live framing test serves a directory and a
// file on one connection) with a fallback for single-entry tests.
type davHeadStorage struct {
	entry    model.RemoteEntry
	byPath   map[string]model.RemoteEntry
	children []model.RemoteEntry
	err      error
	listErr  error
	calls    []string
}

func (f *davHeadStorage) Metadata(path string) (model.RemoteEntry, error) {
	f.calls = append(f.calls, path)
	if f.err != nil {
		return model.RemoteEntry{}, f.err
	}
	if f.byPath != nil {
		if entry, ok := f.byPath[path]; ok {
			return entry, nil
		}
	}
	return f.entry, nil
}

func (f *davHeadStorage) ListPath(path string) ([]model.RemoteEntry, error) {
	f.calls = append(f.calls, "list:"+path)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.children, nil
}

func (f *davHeadStorage) ListChildren(scopePath string, entry model.RemoteEntry) ([]model.RemoteEntry, error) {
	return nil, nil
}

func newDAVRouter(t *testing.T, storage DAVStorage) *Router {
	t.Helper()
	dispatcher, err := NewDAVDispatcher(storage, ControlLimits{}, DAVLimits{}, DownloadLimits{}, stubDownloadStorage{}, stubUploadStorage{}, 0, "/dav")
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
				sendError(w, r, http.StatusNotFound, "unknown REST route", true, nil, false)
				return nil
			},
			DAV: dispatcher.ServeDAV,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func headFileEntry() model.RemoteEntry {
	return model.RemoteEntry{
		ID:   "bench-file-1",
		Name: "bench-one.txt",
		Kind: model.KindFile,
		Size: model.Ptr(int64(11)),
		Etag: model.Ptr("bench-etag-bench-file-1"),
	}
}

// TestDAVHeadFile pins the full _send_download head-branch header set.
func TestDAVHeadFile(t *testing.T) {
	storage := &davHeadStorage{entry: headFileEntry()}
	router := newDAVRouter(t, storage)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("HEAD", "/dav/bench-one.txt"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	header := recorder.Header()
	want := map[string]string{
		"Content-Type":           "text/plain",
		"Accept-Ranges":          "bytes",
		"Cache-Control":          "no-store, no-transform",
		"X-Content-Type-Options": "nosniff",
		"ETag":                   `"bench-etag-bench-file-1"`,
		"Content-Length":         "11",
		"Connection":             "close",
	}
	for name, value := range want {
		if name == "ETag" {
			// The handler writes the raw "ETag" key; Get would look up the
			// canonicalized "Etag" and miss it.
			continue
		}
		if got := header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	if etag := header["ETag"]; len(etag) != 1 || etag[0] != `"bench-etag-bench-file-1"` {
		t.Errorf("ETag = %q, want %q", etag, `"bench-etag-bench-file-1"`)
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", recorder.Body.String())
	}
	if len(storage.calls) != 1 || storage.calls[0] != "/bench-one.txt" {
		t.Errorf("storage calls = %v", storage.calls)
	}
}

// TestDAVHeadEtagQuoting mirrors f'"{etag.strip(chr(34))}"': pre-quoted
// values are stripped and re-quoted exactly once; empty or absent etags
// omit the header.
func TestDAVHeadEtagQuoting(t *testing.T) {
	cases := []struct {
		etag   *string
		want   string
		exists bool
	}{
		{model.Ptr("abc"), `"abc"`, true},
		{model.Ptr(`"abc"`), `"abc"`, true},
		{model.Ptr(`""a""`), `"a"`, true},
		{model.Ptr(""), "", false},
		{nil, "", false},
	}
	for _, tc := range cases {
		entry := headFileEntry()
		entry.Etag = tc.etag
		router := newDAVRouter(t, &davHeadStorage{entry: entry})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("HEAD", "/dav/bench-one.txt"))
		got, ok := recorder.Header()["ETag"]
		if ok != tc.exists || (tc.exists && (len(got) != 1 || got[0] != tc.want)) {
			t.Errorf("etag %v: header = %q (present %v), want %q (present %v)", tc.etag, got, ok, tc.want, tc.exists)
		}
	}
}

// TestDAVHeadDirectory pins the raw directory answer: the fixed type, a
// zero length, and none of the download headers.
func TestDAVHeadDirectory(t *testing.T) {
	router := newDAVRouter(t, &davHeadStorage{entry: model.RemoteEntry{ID: "root", Name: "root", Kind: model.KindFolder, Size: model.Ptr(int64(0))}})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("HEAD", "/dav/"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	header := recorder.Header()
	if got := header.Get("Content-Type"); got != "httpd/unix-directory" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := header.Get("Content-Length"); got != "0" {
		t.Errorf("Content-Length = %q", got)
	}
	for _, name := range []string{"Cache-Control", "ETag", "Accept-Ranges", "Connection"} {
		if got := header.Get(name); got != "" {
			t.Errorf("%s = %q, want absent", name, got)
		}
	}
}

// TestDAVHeadErrors maps metadata failures through the domain table with
// the plain-text framing. A HEAD request never carries the error body —
// Python's _send_bytes skips it for HEAD too — so the text itself is
// pinned by the B602 DAV goldens and here only the status and headers.
func TestDAVHeadErrors(t *testing.T) {
	cases := []struct {
		name       string
		storage    *davHeadStorage
		wantStatus int
		wantRetry  string
	}{
		{
			name:       "not found",
			storage:    &davHeadStorage{err: model.NewStorageError(model.KindEntryNotFound, "entry not found: missing")},
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "not a file",
			storage:    &davHeadStorage{entry: model.RemoteEntry{ID: "x", Name: "x", Kind: model.KindUnknown}},
			wantStatus: http.StatusConflict,
		},
		{
			name:       "upstream 500",
			storage:    &davHeadStorage{err: model.NewWpsAPIError("list entries", 500, model.WpsCategoryUpstream)},
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "upstream 401",
			storage:    &davHeadStorage{err: model.NewWpsAPIError("list entries", 401, model.WpsCategorySessionExpired)},
			wantStatus: http.StatusServiceUnavailable,
			wantRetry:  "60",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := newDAVRouter(t, tc.storage)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, newTestRequest("HEAD", "/dav/x"))
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
			}
			if recorder.Body.Len() != 0 {
				t.Errorf("HEAD error body = %q, want empty", recorder.Body.String())
			}
			if got := recorder.Header().Get("Retry-After"); got != tc.wantRetry {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetry)
			}
			// Error framing keeps the control no-store but must not claim
			// the download cache rule.
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
		})
	}
}

// TestDAVUnknownDAVMethods pins the interim answer of the DAV methods
// whose stages have not landed yet; GET streams downloads since B801.
func TestDAVUnknownDAVMethods(t *testing.T) {
	router := newDAVRouter(t, &davHeadStorage{entry: headFileEntry()})
	for _, method := range []string{"MKCOL", "LOCK"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest(method, "/dav/bench-one.txt"))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d", method, recorder.Code)
		}
		if recorder.Body.String() != "unknown route\n" {
			t.Errorf("%s body = %q", method, recorder.Body.String())
		}
	}
}

// TestDAVHeadContractGoldens replays the recorded Python HEAD responses.
func TestDAVHeadContractGoldens(t *testing.T) {
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
	t.Run("DAV-HEAD-001 file", func(t *testing.T) {
		want := record("DAV-HEAD-001")
		router := newDAVRouter(t, &davHeadStorage{entry: headFileEntry()})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("HEAD", "/dav/bench-one.txt"))
		if recorder.Code != int(want["status"].(float64)) {
			t.Fatalf("status = %d, want %v", recorder.Code, want["status"])
		}
		if recorder.Body.Len() != int(want["body_bytes"].(float64)) {
			t.Errorf("body bytes = %d", recorder.Body.Len())
		}
		if got := recorder.Header().Get("Content-Length"); got != want["content_length"] {
			t.Errorf("Content-Length = %q, want %q", got, want["content_length"])
		}
		if got := recorder.Header()["ETag"]; len(got) != 1 || got[0] != want["etag"] {
			t.Errorf("ETag = %q, want %q", got, want["etag"])
		}
	})
	t.Run("DAV-HEAD-002 directory", func(t *testing.T) {
		want := record("DAV-HEAD-002")
		router := newDAVRouter(t, &davHeadStorage{entry: model.RemoteEntry{ID: "root", Name: "root", Kind: model.KindFolder}})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("HEAD", "/dav/"))
		if recorder.Code != int(want["status"].(float64)) {
			t.Fatalf("status = %d, want %v", recorder.Code, want["status"])
		}
		if recorder.Body.Len() != int(want["body_bytes"].(float64)) {
			t.Errorf("body bytes = %d", recorder.Body.Len())
		}
		if got := recorder.Header().Get("Content-Length"); got != want["content_length"] {
			t.Errorf("Content-Length = %q, want %q", got, want["content_length"])
		}
		if got := recorder.Header().Get("Content-Type"); got != want["content_type"] {
			t.Errorf("Content-Type = %q, want %q", got, want["content_type"])
		}
	})
}

// TestDAVHeadLiveFraming probes the real transport (the curl view): a file
// HEAD closes the connection after the empty body, a directory HEAD keeps
// the connection alive for the next request.
func TestDAVHeadLiveFraming(t *testing.T) {
	dirEntry := model.RemoteEntry{ID: "root", Name: "root", Kind: model.KindFolder, Size: model.Ptr(int64(0))}
	storage := &davHeadStorage{byPath: map[string]model.RemoteEntry{
		"/":              dirEntry,
		"/bench-one.txt": headFileEntry(),
	}}
	router := newDAVRouter(t, storage)
	server := httptest.NewServer(router)
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")

	// File HEAD: response then EOF.
	response := rawExchange(t, address, "HEAD /dav/bench-one.txt HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 200") {
		t.Fatalf("file head status: %s", response)
	}
	// The wire keeps Python's exact "ETag" spelling, not Go's "Etag".
	if !strings.Contains(response, "ETag: \"bench-etag-bench-file-1\"\r\n") {
		t.Errorf("file head ETag missing: %s", response)
	}
	if !strings.Contains(response, "Content-Length: 11") {
		t.Errorf("file head length missing: %s", response)
	}
	if body := response[strings.Index(response, "\r\n\r\n")+4:]; body != "" {
		t.Errorf("file head must carry no body: %q", body)
	}

	// Capability probe (the curl view): exact DAV and Allow capability
	// headers with Python's raw-case "DAV" spelling.
	response = rawExchange(t, address, "OPTIONS /dav/ HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if !strings.Contains(response, "DAV: 1,2\r\n") {
		t.Errorf("OPTIONS DAV header: %s", response)
	}
	if !strings.Contains(response, "Allow: OPTIONS, PROPFIND, GET, HEAD, PUT, MKCOL, DELETE, MOVE, COPY, LOCK, UNLOCK\r\n") {
		t.Errorf("OPTIONS Allow header: %s", response)
	}

	// Directory HEAD then file HEAD on one connection: two answers, the
	// connection only closes after the second (the download close rule).
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("HEAD /dav/ HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	first := readOneResponse(t, conn)
	if !strings.HasPrefix(first, "HTTP/1.1 200") || !strings.Contains(first, "httpd/unix-directory") {
		t.Fatalf("directory head response: %s", first)
	}
	if _, err := conn.Write([]byte("HEAD /dav/bench-one.txt HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	second, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(second), "HTTP/1.1 200") || !strings.Contains(string(second), "Content-Length: 11") {
		t.Fatalf("second response: %s", second)
	}
}

// readOneResponse reads a single response's header block from a
// keep-alive connection; HEAD answers carry no body bytes, so the block
// is the whole response.
func readOneResponse(t *testing.T, conn net.Conn) string {
	t.Helper()
	var buf strings.Builder
	chunk := make([]byte, 1024)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			if text := buf.String(); strings.Contains(text, "\r\n\r\n") {
				return text
			}
		}
		if err != nil {
			t.Fatalf("reading response: %v", err)
		}
	}
}
