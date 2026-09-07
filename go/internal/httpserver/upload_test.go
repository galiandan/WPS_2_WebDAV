package httpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

// fakeUploadStorage records the upload the dispatcher handed over so tests
// can assert the exact path, options, and body stream.
type fakeUploadStorage struct {
	entry     model.RemoteEntry
	err       error
	size      *int64
	name      string
	body      []byte
	contents  string
	overwrite bool
}

func (f *fakeUploadStorage) UploadPath(ctx context.Context, path string, source io.Reader, options storage.UploadOptions) (model.RemoteEntry, error) {
	f.body, _ = io.ReadAll(source)
	if f.err != nil {
		return model.RemoteEntry{}, f.err
	}
	f.name = path
	f.size = options.Size
	f.overwrite = options.Overwrite
	if f.contents != "" {
		f.body = []byte(f.contents)
	}
	if f.entry.ID == "" {
		f.entry = model.RemoteEntry{ID: "file-1", Name: "uploaded.txt", Kind: model.KindFile, Size: model.Ptr(int64(len(f.body)))}
	}
	return f.entry, nil
}

func newUploadRouter(t *testing.T, uploads UploadStorage, maxUploadBytes int64) *Router {
	t.Helper()
	limits := ControlLimits{MaxControlBody: 64 * 1024, MaxResponseBody: 64 * 1024}
	session, err := NewSessionImporter(limits, "",
		func([]any, string) (string, string, []string, error) {
			return "", "", nil, errBadRequest("unused in upload tests")
		},
		&recordingCredentialReplacer{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewRootNameController(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewRESTDispatcher(limits, controller, session, &fakeReadStorage{}, nil, DownloadLimits{}, stubDownloadStorage{}, uploads, stubMutations{}, newTestLockStore(t), maxUploadBytes)
	if err != nil {
		t.Fatal(err)
	}
	dav, err := NewDAVDispatcher(&davHeadStorage{}, ControlLimits{}, DAVLimits{}, DownloadLimits{}, stubDownloadStorage{}, uploads, stubMutations{}, newTestLockStore(t), maxUploadBytes, "/dav")
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
			REST:     dispatcher.ServeREST,
			DAV:      dav.ServeDAV,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func restUploadRequest(method, target string, body []byte) *http.Request {
	if body == nil {
		return httptest.NewRequest(method, target, nil)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	// httptest.NewRequest sets only the ContentLength field, so mirror the
	// wire header the server would have parsed.
	request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return request
}

func TestRestPutRequiresContentLengthBeforeReadingBody(t *testing.T) {
	router := newUploadRouter(t, &fakeUploadStorage{}, 0)
	// httptest.NewRequest supplies a real Content-Length, so strip it to
	// simulate the missing declaration.
	request := restUploadRequest(http.MethodPut, "/api/v1/upload?path=/a.txt", []byte("x"))
	request.ContentLength = 0
	request.Header.Del("Content-Length")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusLengthRequired {
		t.Fatalf("status = %d, want 411", recorder.Code)
	}
	if got := recorder.Header().Get("Connection"); got != "close" {
		t.Fatalf("Connection = %q, want close", got)
	}
	if got := recorder.Body.String(); got != "Content-Length is required\n" {
		t.Fatalf("body = %q", got)
	}
}

func TestRestPutRefusesDeclaredLengthOverMaxBeforeReadingBody(t *testing.T) {
	uploads := &fakeUploadStorage{}
	router := newUploadRouter(t, uploads, 16)
	request := restUploadRequest(http.MethodPut, "/api/v1/upload?path=/a.txt", []byte("way more than sixteen bytes on the wire"))
	request.Header.Set("Content-Length", "100000")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", recorder.Code)
	}
	if got := recorder.Header().Get("Connection"); got != "close" {
		t.Fatalf("Connection = %q, want close", got)
	}
	if got := recorder.Body.String(); !strings.Contains(got, `"error":"upload exceeds the configured size limit"`) {
		t.Fatalf("body = %q", got)
	}
	if len(uploads.body) != 0 {
		t.Fatalf("the body was read despite the refusal: %d bytes", len(uploads.body))
	}
}

func TestRestPutParsesPathAndOverwriteQuery(t *testing.T) {
	uploads := &fakeUploadStorage{}
	router := newUploadRouter(t, uploads, 0)
	request := restUploadRequest(http.MethodPut, "/api/v1/upload?path=%2Fdocs%2Fa.txt&overwrite=true", []byte("body"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", recorder.Code)
	}
	if uploads.name != "/docs/a.txt" {
		t.Fatalf("path = %q", uploads.name)
	}
	if uploads.size == nil || *uploads.size != int64(len("body")) {
		t.Fatalf("size = %v", uploads.size)
	}
	if !uploads.overwrite {
		t.Fatal("overwrite=true did not reach the upload storage")
	}
	want := `{"path":"/docs/a.txt","entry":{"id":"file-1","name":"uploaded.txt","kind":"file","parent_id":null,"size":4,"modified_at":null,"etag":null}}`
	if got := recorder.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestRestPutDefaultsToNoOverwrite(t *testing.T) {
	uploads := &fakeUploadStorage{}
	router := newUploadRouter(t, uploads, 0)
	request := restUploadRequest(http.MethodPut, "/api/v1/upload?path=%2Fdocs%2Fa.txt", []byte("body"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", recorder.Code)
	}
	if uploads.overwrite {
		t.Fatal("a REST upload without the overwrite query must default to no overwrite")
	}
}

func TestRestPutRejectsBadOverwriteQuery(t *testing.T) {
	uploads := &fakeUploadStorage{}
	router := newUploadRouter(t, uploads, 0)
	request := restUploadRequest(http.MethodPut, "/api/v1/upload?path=%2Fa.txt&overwrite=maybe", []byte("body"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if got := recorder.Body.String(); !strings.Contains(got, "query parameter 'overwrite' must be boolean") {
		t.Fatalf("body = %q", got)
	}
	if uploads.name != "" {
		t.Fatalf("the upload storage was reached: %q", uploads.name)
	}
}

func TestRestPutUnknownSuffixDiscardsBodyAndAnswers404(t *testing.T) {
	uploads := &fakeUploadStorage{}
	router := newUploadRouter(t, uploads, 0)
	request := restUploadRequest(http.MethodPut, "/api/v1/other?path=%2Fa.txt", []byte("body"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if uploads.name != "" {
		t.Fatalf("the upload storage was reached: %q", uploads.name)
	}
}

func TestRestPutMapsUploadErrorsThroughTheDomainTable(t *testing.T) {
	uploads := &fakeUploadStorage{err: model.NewStorageError(model.KindBadRequest, "source size mismatch: expected 5, read 3")}
	router := newUploadRouter(t, uploads, 0)
	request := restUploadRequest(http.MethodPut, "/api/v1/upload?path=%2Fa.txt", []byte("abc"))
	request.Header.Set("Content-Length", "5")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if got := recorder.Body.String(); !strings.Contains(got, `"error":"source size mismatch: expected 5, read 3"`) {
		t.Fatalf("body = %q", got)
	}
}

func TestDavPutUploadsWithOverwriteAndLocation(t *testing.T) {
	uploads := &fakeUploadStorage{}
	router := newUploadRouter(t, uploads, 0)
	request := restUploadRequest(http.MethodPut, "/dav/a.txt", []byte("body"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", recorder.Code)
	}
	if uploads.name != "/a.txt" {
		t.Fatalf("path = %q", uploads.name)
	}
	if uploads.size == nil || *uploads.size != 4 {
		t.Fatalf("size = %v", uploads.size)
	}
	if !uploads.overwrite {
		t.Fatal("a DAV PUT must always upload with overwrite")
	}
	if got := recorder.Header().Get("Location"); got != "/dav/a.txt" {
		t.Fatalf("Location = %q", got)
	}
	want := `{"id":"file-1","name":"uploaded.txt","kind":"file","parent_id":null,"size":4,"modified_at":null,"etag":null}`
	if got := recorder.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestDavPutRefusesDeclaredLengthOverMax(t *testing.T) {
	uploads := &fakeUploadStorage{}
	router := newUploadRouter(t, uploads, 4)
	request := restUploadRequest(http.MethodPut, "/dav/a.txt", []byte("body"))
	request.Header.Set("Content-Length", "5")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", recorder.Code)
	}
	if got := recorder.Header().Get("Connection"); got != "close" {
		t.Fatalf("Connection = %q, want close", got)
	}
	if got := recorder.Body.String(); got != "upload exceeds the configured size limit\n" {
		t.Fatalf("body = %q", got)
	}
	if uploads.name != "" {
		t.Fatalf("the upload storage was reached: %q", uploads.name)
	}
}

func TestDavPutRequiresContentLength(t *testing.T) {
	uploads := &fakeUploadStorage{}
	router := newUploadRouter(t, uploads, 0)
	request := restUploadRequest(http.MethodPut, "/dav/a.txt", []byte("body"))
	request.ContentLength = 0
	request.Header.Del("Content-Length")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusLengthRequired {
		t.Fatalf("status = %d, want 411", recorder.Code)
	}
	if got := recorder.Body.String(); got != "Content-Length is required\n" {
		t.Fatalf("body = %q", got)
	}
}

// stubUploadStorage refuses every upload; routes that reach it fail loudly
// instead of silently succeeding.
type stubUploadStorage struct{}

func (stubUploadStorage) UploadPath(context.Context, string, io.Reader, storage.UploadOptions) (model.RemoteEntry, error) {
	return model.RemoteEntry{}, model.NewStorageError(model.KindUnsupportedOperation, "upload is not wired in this test")
}

func TestLimitedUploadBodyClampsToTheDeclaredLength(t *testing.T) {
	source := &cappedReader{data: "0123456789"}
	reader := &limitedUploadBody{source: source, remaining: 4}
	first := make([]byte, 3)
	if _, err := io.ReadFull(reader, first); err != nil {
		t.Fatalf("ReadFull failed: %v", err)
	}
	// A read asking beyond the remaining declared length is clamped, even
	// though the source would deliver more.
	rest := make([]byte, 32)
	n, err := reader.Read(rest)
	if err != nil || string(rest[:n]) != "3" {
		t.Fatalf("clamped read = %q, %v; want %q", rest[:n], err, "3")
	}
	if _, err := reader.Read(rest); !errors.Is(err, io.EOF) {
		t.Fatalf("read past the declared length = %v, want EOF", err)
	}
	if source.reads != 2 {
		t.Fatalf("source reads = %d, want the clamp to stop after the second read", source.reads)
	}
}

func TestLimitedUploadBodyMapsUnexpectedEOFCleanly(t *testing.T) {
	reader := &limitedUploadBody{source: &truncatedReader{}, remaining: 8}
	buf := make([]byte, 8)
	n, err := reader.Read(buf)
	if err != nil || n != 3 || string(buf[:n]) != "abc" {
		t.Fatalf("read = %q, %v; want %q", buf[:n], err, "abc")
	}
	if _, err := reader.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("second read = %v, want the transport error mapped to EOF", err)
	}
}

// cappedReader serves the whole buffer per read, so a read larger than the
// remaining declared length would over-deliver without the clamp.
type cappedReader struct {
	data  string
	reads int
}

func (r *cappedReader) Read(p []byte) (int, error) {
	r.reads++
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// truncatedReader hands out three bytes, then reports the connection as
// gone mid-body the way net/http does.
type truncatedReader struct {
	done bool
}

func (r *truncatedReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	r.done = true
	return copy(p, "abc"), nil
}
