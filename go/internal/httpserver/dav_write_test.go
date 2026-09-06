package httpserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

// recordingMutations records every write and answers with a fixed entry;
// it satisfies both dispatcher mutation interfaces.
type recordingMutations struct {
	folders []string
	deletes []string
	renames [][2]string
	moves   [][2]string
	copies  []copyCall

	entry model.RemoteEntry
	err   error
}

type copyCall struct {
	source      string
	destination string
	options     storage.CopyOptions
}

func (m *recordingMutations) CreateFolderPath(path string) (model.RemoteEntry, error) {
	m.folders = append(m.folders, path)
	return m.answer(), m.err
}

func (m *recordingMutations) DeletePath(path string) error {
	m.deletes = append(m.deletes, path)
	return m.err
}

func (m *recordingMutations) RenamePath(path string, name string) (model.RemoteEntry, error) {
	m.renames = append(m.renames, [2]string{path, name})
	return m.answer(), m.err
}

func (m *recordingMutations) MovePath(path string, destination string) (model.RemoteEntry, error) {
	m.moves = append(m.moves, [2]string{path, destination})
	return m.answer(), m.err
}

func (m *recordingMutations) MoveToParentPath(path string, parentPath string) (model.RemoteEntry, error) {
	m.moves = append(m.moves, [2]string{path, "parent:" + parentPath})
	return m.answer(), m.err
}

func (m *recordingMutations) CopyPath(ctx context.Context, source string, destination string, options storage.CopyOptions) (model.RemoteEntry, error) {
	m.copies = append(m.copies, copyCall{source: source, destination: destination, options: options})
	return m.answer(), m.err
}

func (m *recordingMutations) answer() model.RemoteEntry {
	if m.entry.ID != "" {
		return m.entry
	}
	return model.RemoteEntry{ID: "new-1", Name: "new", Kind: model.KindFile, ParentID: model.Ptr("parent"), Size: model.Ptr(int64(3))}
}

// mapEntryStorage answers metadata strictly by path, like the write routes
// expect: anything unlisted is entry-not-found.
type mapEntryStorage struct {
	byPath map[string]model.RemoteEntry
}

func (f *mapEntryStorage) Metadata(path string) (model.RemoteEntry, error) {
	if entry, ok := f.byPath[path]; ok {
		return entry, nil
	}
	return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "entry not found: "+path)
}

func (f *mapEntryStorage) ListPath(path string) ([]model.RemoteEntry, error) {
	return nil, model.NewStorageError(model.KindNotFolder, "not a folder: "+path)
}

func (f *mapEntryStorage) ListChildren(scopePath string, entry model.RemoteEntry) ([]model.RemoteEntry, error) {
	return nil, nil
}

// newWriteRouter wires both dispatchers behind the real router so error
// mapping runs exactly like production.
func newWriteRouter(t *testing.T, byPath map[string]model.RemoteEntry, mutations *recordingMutations, uploads UploadStorage) (*Router, *DavLockStore) {
	t.Helper()
	if mutations == nil {
		mutations = &recordingMutations{}
	}
	if uploads == nil {
		uploads = stubUploadStorage{}
	}
	locks := newTestLockStore(t)
	limits := ControlLimits{MaxControlBody: 64 * 1024, MaxResponseBody: 16 * 1024 * 1024}
	dav, err := NewDAVDispatcher(&mapEntryStorage{byPath: byPath}, limits,
		DAVLimits{}, DownloadLimits{}, stubDownloadStorage{}, uploads, mutations, locks, 0, "/dav")
	if err != nil {
		t.Fatal(err)
	}
	rest, err := NewRESTDispatcher(limits, nil, &SessionImporter{}, &mapEntryStorage{byPath: byPath}, nil,
		DownloadLimits{}, stubDownloadStorage{}, uploads, mutations, locks, 0)
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
			REST:     rest.ServeREST,
			DAV:      dav.ServeDAV,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return router, locks
}

// writeRequest builds a request with an explicit Content-Length header.
func writeRequest(method, target string, headers map[string]string, body string) *http.Request {
	request := newTestRequest(method, target)
	if body != "" {
		request.Body = io.NopCloser(strings.NewReader(body))
		request.ContentLength = int64(len(body))
		request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return request
}

func fileEntryAt(name string) model.RemoteEntry {
	return model.RemoteEntry{ID: "id-" + name, Name: name, Kind: model.KindFile, ParentID: model.Ptr("root"), Size: model.Ptr(int64(3))}
}

func rootFolder() model.RemoteEntry {
	return model.RemoteEntry{ID: "root", Name: "root", Kind: model.KindFolder, Size: model.Ptr(int64(0))}
}

func lockInfoBody(owner string) string {
	if owner == "" {
		return `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>`
	}
	return `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:owner>` + owner + `</D:owner></D:lockinfo>`
}

func lockTokenOf(recorder *httptest.ResponseRecorder) string {
	return strings.TrimSuffix(strings.TrimPrefix(recorder.Header().Get("Lock-Token"), "<"), ">")
}

func TestDavMkcolCreatesAndLocks(t *testing.T) {
	mutations := &recordingMutations{entry: model.RemoteEntry{ID: "f1", Name: "new-dir", Kind: model.KindFolder, ParentID: model.Ptr("root"), Size: model.Ptr(int64(0))}}
	router, locks := newWriteRouter(t, map[string]model.RemoteEntry{"/": rootFolder()}, mutations, nil)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("MKCOL", "/dav/new-dir", nil, ""))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Location") != "/dav/new-dir/" {
		t.Fatalf("location = %q", recorder.Header().Get("Location"))
	}
	if recorder.Body.String() != `{"id":"f1","name":"new-dir","kind":"folder","parent_id":"root","size":0,"modified_at":null,"etag":null}` {
		t.Fatalf("body = %q", recorder.Body.String())
	}
	if len(mutations.folders) != 1 || mutations.folders[0] != "/new-dir" {
		t.Fatalf("folders = %v", mutations.folders)
	}

	// A depth-infinity lock on the root blocks the creation; the presenting
	// token releases it.
	active, err := locks.Acquire("/", "infinity", "owner", 600, "")
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("MKCOL", "/dav/inner", nil, ""))
	if recorder.Code != http.StatusLocked || recorder.Body.String() != "resource is locked\n" {
		t.Fatalf("locked MKCOL = (%d, %q)", recorder.Code, recorder.Body.String())
	}
	if len(mutations.folders) != 1 {
		t.Fatal("a locked MKCOL must not reach storage")
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("MKCOL", "/dav/inner", map[string]string{"If": "<" + active.Token + ">"}, ""))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("token MKCOL = %d", recorder.Code)
	}
}

func TestDavDeleteAnswers204(t *testing.T) {
	mutations := &recordingMutations{}
	router, _ := newWriteRouter(t, map[string]model.RemoteEntry{"/bench-one.txt": fileEntryAt("bench-one.txt")}, mutations, nil)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("DELETE", "/dav/bench-one.txt", nil, ""))
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 {
		t.Fatalf("delete = (%d, %q)", recorder.Code, recorder.Body.String())
	}
	if len(mutations.deletes) != 1 || mutations.deletes[0] != "/bench-one.txt" {
		t.Fatalf("deletes = %v", mutations.deletes)
	}
}

func TestDavDestinationValidation(t *testing.T) {
	router, _ := newWriteRouter(t, map[string]model.RemoteEntry{"/bench-one.txt": fileEntryAt("bench-one.txt")}, nil, nil)
	cases := map[string]string{
		"missing":        "",
		"query":          "/dav/x?query=1",
		"fragment":       "/dav/x#frag",
		"userinfo":       "https://user:pw@127.0.0.1/dav/x",
		"other host":     "https://evil.example/dav/x",
		"other port":     "http://127.0.0.1:9/dav/x",
		"outside prefix": "/api/v1/x",
	}
	for label, destination := range cases {
		headers := map[string]string{}
		if destination != "" {
			headers["Destination"] = destination
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, writeRequest("MOVE", "/dav/bench-one.txt", headers, ""))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400 (body %q)", label, recorder.Code, recorder.Body.String())
		}
	}
}

func TestDavDestinationPercentDecodedOnce(t *testing.T) {
	mutations := &recordingMutations{}
	router, _ := newWriteRouter(t, map[string]model.RemoteEntry{"/bench-one.txt": fileEntryAt("bench-one.txt")}, mutations, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("MOVE", "/dav/bench-one.txt", map[string]string{"Destination": "/dav/bench%20two.txt"}, ""))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if len(mutations.moves) != 1 || mutations.moves[0] != [2]string{"/bench-one.txt", "/bench two.txt"} {
		t.Fatalf("moves = %v", mutations.moves)
	}
	if recorder.Header().Get("Location") != "/dav/bench%20two.txt" {
		t.Fatalf("location = %q", recorder.Header().Get("Location"))
	}
}

func TestDavMoveOverwriteSemantics(t *testing.T) {
	storage := map[string]model.RemoteEntry{
		"/bench-one.txt": fileEntryAt("bench-one.txt"),
		"/bench-two.txt": fileEntryAt("bench-two.txt"),
	}
	mutations := &recordingMutations{}
	router, _ := newWriteRouter(t, storage, mutations, nil)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("MOVE", "/dav/bench-one.txt", map[string]string{"Destination": "/dav/bench-two.txt"}, ""))
	if recorder.Code != http.StatusNotImplemented ||
		recorder.Body.String() != "MOVE overwrite is disabled because WPS move is not atomic\n" {
		t.Fatalf("default overwrite = (%d, %q)", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("MOVE", "/dav/bench-one.txt", map[string]string{"Destination": "/dav/bench-two.txt", "Overwrite": "F"}, ""))
	if recorder.Code != http.StatusPreconditionFailed || recorder.Body.String() != "destination already exists\n" {
		t.Fatalf("overwrite F = (%d, %q)", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("MOVE", "/dav/bench-one.txt", map[string]string{"Destination": "/dav/x", "Overwrite": "X"}, ""))
	if recorder.Code != http.StatusBadRequest || recorder.Body.String() != "Overwrite must be T or F\n" {
		t.Fatalf("bad overwrite = (%d, %q)", recorder.Code, recorder.Body.String())
	}
	if len(mutations.moves) != 0 {
		t.Fatal("no move may reach storage when the destination exists")
	}

	// A fresh destination answers 201 with the entry JSON and Location.
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("MOVE", "/dav/bench-one.txt", map[string]string{"Destination": "/dav/bench-three.txt"}, ""))
	if recorder.Code != http.StatusCreated || recorder.Header().Get("Location") != "/dav/bench-three.txt" {
		t.Fatalf("fresh move = (%d, %q)", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestDavCopyDepthOverwriteAndRelay(t *testing.T) {
	storage := map[string]model.RemoteEntry{
		"/bench-one.txt": fileEntryAt("bench-one.txt"),
		"/bench-two.txt": fileEntryAt("bench-two.txt"),
	}
	mutations := &recordingMutations{}
	router, _ := newWriteRouter(t, storage, mutations, nil)

	// Invalid depth discards the body and answers 400.
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("COPY", "/dav/bench-one.txt", map[string]string{"Destination": "/dav/copy", "Depth": "2"}, "junk"))
	if recorder.Code != http.StatusBadRequest || recorder.Body.String() != "Depth must be 0, 1 or infinity\n" {
		t.Fatalf("bad depth = (%d, %q)", recorder.Code, recorder.Body.String())
	}
	// Existing destination: default 501, Overwrite F 412.
	for label, headers := range map[string]map[string]string{
		"default":     {"Destination": "/dav/bench-two.txt"},
		"overwrite F": {"Destination": "/dav/bench-two.txt", "Overwrite": "F"},
	} {
		recorder = httptest.NewRecorder()
		router.ServeHTTP(recorder, writeRequest("COPY", "/dav/bench-one.txt", headers, ""))
		want := http.StatusNotImplemented
		if label == "overwrite F" {
			want = http.StatusPreconditionFailed
		}
		if recorder.Code != want {
			t.Fatalf("%s copy = %d, want %d", label, recorder.Code, want)
		}
	}
	if len(mutations.copies) != 0 {
		t.Fatal("an existing destination must never be copied onto, let alone deleted")
	}

	// A fresh copy reaches storage with the normalized depth and default
	// overwrite.
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("COPY", "/dav/bench-one.txt",
		map[string]string{"Destination": "/dav/bench-copy.txt", "Depth": "Infinity"}, ""))
	if recorder.Code != http.StatusCreated || recorder.Header().Get("Location") != "/dav/bench-copy.txt" {
		t.Fatalf("fresh copy = (%d, %q)", recorder.Code, recorder.Header().Get("Location"))
	}
	if len(mutations.copies) != 1 || mutations.copies[0].source != "/bench-one.txt" ||
		mutations.copies[0].destination != "/bench-copy.txt" || mutations.copies[0].options.Depth != "infinity" ||
		!mutations.copies[0].options.Overwrite {
		t.Fatalf("copies = %+v", mutations.copies)
	}

	// The storage-side already-exists race answers 412 again without
	// overwrite.
	mutations.err = model.NewStorageError(model.KindAlreadyExists, "entry already exists: /bench-race.txt")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("COPY", "/dav/bench-one.txt",
		map[string]string{"Destination": "/dav/bench-race.txt", "Overwrite": "F"}, ""))
	if recorder.Code != http.StatusPreconditionFailed {
		t.Fatalf("race copy = %d", recorder.Code)
	}
}

func TestDavLockNewLockLifecycle(t *testing.T) {
	router, _ := newWriteRouter(t, map[string]model.RemoteEntry{"/bench-one.txt": fileEntryAt("bench-one.txt")}, nil, nil)

	// A new lock on an existing resource answers 200 with the discovery XML.
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("LOCK", "/dav/bench-one.txt", map[string]string{"Content-Type": "application/xml"}, lockInfoBody("bench owner")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("lock status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if dav := recorder.Header()["DAV"]; len(dav) != 1 || dav[0] != "1,2" {
		t.Fatalf("dav header = %v", dav)
	}
	token := lockTokenOf(recorder)
	if !strings.HasPrefix(token, "opaquelocktoken:") {
		t.Fatalf("token header = %q", recorder.Header().Get("Lock-Token"))
	}
	responseBody := recorder.Body.String()
	if !strings.HasPrefix(responseBody, "<?xml version='1.0' encoding='utf-8'?>\n<prop xmlns:D=\"DAV:\"><D:lockdiscovery><D:activelock><D:locktype><D:write /></D:locktype><D:lockscope><D:exclusive /></D:lockscope><D:depth>Infinity</D:depth><D:owner>bench owner</D:owner><D:timeout>Second-") ||
		!strings.HasSuffix(responseBody,
			"</D:timeout><D:locktoken><D:href>"+token+"</D:href></D:locktoken><D:lockroot><D:href>/dav/bench-one.txt</D:href></D:lockroot></D:activelock></D:lockdiscovery></prop>") {
		t.Fatalf("lock body = %q", responseBody)
	}

	// A refresh with the If header keeps the token and answers 200.
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("LOCK", "/dav/bench-one.txt", map[string]string{"If": "<" + token + ">"}, ""))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), token) {
		t.Fatalf("refresh = (%d, %q)", recorder.Code, recorder.Body.String())
	}

	// A new lock on a missing resource answers 201.
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("LOCK", "/dav/lock-null.txt", nil, lockInfoBody("")))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("lock-null status = %d", recorder.Code)
	}
}

func TestDavLockRequestValidation(t *testing.T) {
	router, _ := newWriteRouter(t, map[string]model.RemoteEntry{"/bench-one.txt": fileEntryAt("bench-one.txt")}, nil, nil)

	cases := []struct {
		name    string
		headers map[string]string
		body    string
		status  int
		message string
	}{
		{name: "depth 1", headers: map[string]string{"Depth": "1"}, body: lockInfoBody(""), status: 400, message: "LOCK Depth must be 0 or infinity\n"},
		{name: "bad timeout", headers: map[string]string{"Timeout": "Second-abc"}, body: lockInfoBody(""), status: 400, message: "Timeout must be Second-N or Infinite\n"},
		{name: "doctype", body: `<?xml version="1.0"?><!DOCTYPE lockinfo [<!ENTITY x "y">]><D:lockinfo xmlns:D="DAV:"/>`, status: 400, message: "LOCK request body must not declare XML entities\n"},
		{name: "not xml", body: "not xml", status: 400, message: "LOCK request body must be valid XML\n"},
		{name: "unclosed", body: `<D:lockinfo><D:owner>a</D:lockinfo>`, status: 400, message: "LOCK request body must be valid XML\n"},
		{name: "second root", body: `<a/><b/>`, status: 400, message: "LOCK request body must be valid XML\n"},
		{name: "short body", body: lockInfoBody("")[:4], status: 400, message: "request body is shorter than Content-Length\n"},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		request := writeRequest("LOCK", "/dav/bench-one.txt", tc.headers, tc.body)
		if tc.name == "short body" {
			// The declared length exceeds the provided bytes.
			request.ContentLength = 99
			request.Header.Set("Content-Length", "99")
		}
		router.ServeHTTP(recorder, request)
		if recorder.Code != tc.status || recorder.Body.String() != tc.message {
			t.Fatalf("%s = (%d, %q), want (%d, %q)", tc.name, recorder.Code, recorder.Body.String(), tc.status, tc.message)
		}
	}

	// Multiple lock tokens across If and Lock-Token are refused.
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("LOCK", "/dav/bench-one.txt",
		map[string]string{"If": "<opaquelocktoken:a>", "Lock-Token": "<opaquelocktoken:b>"}, lockInfoBody("")))
	if recorder.Code != 400 || recorder.Body.String() != "LOCK request contains multiple lock tokens\n" {
		t.Fatalf("multi token = (%d, %q)", recorder.Code, recorder.Body.String())
	}

	// Timeout clamping: Infinite and a huge Second-N both land at the cap.
	// Distinct paths keep the two locks independent.
	for label, timeout := range map[string]string{"infinite": "Infinite", "huge": "Second-999999999"} {
		recorder = httptest.NewRecorder()
		router.ServeHTTP(recorder, writeRequest("LOCK", "/dav/lock-"+label+".txt",
			map[string]string{"Timeout": timeout}, lockInfoBody("")))
		if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
			t.Fatalf("%s lock = (%d, %q)", label, recorder.Code, recorder.Body.String())
		}
		response := recorder.Body.String()
		start := strings.Index(response, "Second-") + len("Second-")
		end := strings.Index(response[start:], "<") + start
		if response[start:end] != "86400" && response[start:end] != "86399" {
			t.Fatalf("%s timeout = %q", label, response[start:end])
		}
	}

	// An owner body beyond 64 KiB closes the connection with 413.
	recorder = httptest.NewRecorder()
	huge := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:owner>` + strings.Repeat("x", 65*1024) + `</D:owner></D:lockinfo>`
	router.ServeHTTP(recorder, writeRequest("LOCK", "/dav/bench-huge.txt", nil, huge))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge owner = %d", recorder.Code)
	}
}

func TestDavLockOwnerExtraction(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`<D:lockinfo xmlns:D="DAV:"><D:owner>  spaced
		    owner  </D:owner></D:lockinfo>`, "spaced owner"},
		{`<owner xmlns="DAV:">default ns</owner>`, "default ns"},
		{`<a><b><owner>nested</owner></b></a>`, "nested"},
		{`<a><other>ignored</other><owner>first</owner><owner>second</owner></a>`, "first"},
		{`<a><owner>with <b>child text</b> around</owner></a>`, "with child text around"},
		{`<a/>`, ""},
		{`<a><owner></owner></a>`, ""},
	}
	for _, tc := range cases {
		got, err := lockOwnerFromBody([]byte(tc.body))
		if err != nil {
			t.Fatalf("%q owner error = %v", tc.body, err)
		}
		if got != tc.want {
			t.Fatalf("%q owner = %q, want %q", tc.body, got, tc.want)
		}
	}
	owner, err := lockOwnerFromBody([]byte(`<a><owner>` + strings.Repeat("字", 600) + `</owner></a>`))
	if err != nil {
		t.Fatal(err)
	}
	if runes := []rune(owner); len(runes) != 512 {
		t.Fatalf("owner runes = %d, want 512", len(runes))
	}
}

func TestDavLockInheritanceAndConflicts(t *testing.T) {
	storage := map[string]model.RemoteEntry{
		"/":              rootFolder(),
		"/bench-one.txt": fileEntryAt("bench-one.txt"),
		"/bench-two.txt": fileEntryAt("bench-two.txt"),
	}
	router, _ := newWriteRouter(t, storage, nil, nil)

	// Two sibling file locks coexist.
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("LOCK", "/dav/bench-one.txt", nil, lockInfoBody("")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("first lock = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("LOCK", "/dav/bench-two.txt", nil, lockInfoBody("")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("sibling lock = %d", recorder.Code)
	}
	// Locking an ancestor while a descendant lock exists conflicts.
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("LOCK", "/dav/", nil, lockInfoBody("")))
	if recorder.Code != http.StatusLocked || recorder.Body.String() != "resource is locked\n" {
		t.Fatalf("ancestor lock = (%d, %q)", recorder.Code, recorder.Body.String())
	}

	// A folder lock covers descendant writes; the folder token releases
	// them.
	router2, _ := newWriteRouter(t, storage, nil, &fakeUploadStorage{})
	recorder = httptest.NewRecorder()
	router2.ServeHTTP(recorder, writeRequest("LOCK", "/dav/bench-folder/", nil, lockInfoBody("")))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("folder lock = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	folderToken := lockTokenOf(recorder)
	recorder = httptest.NewRecorder()
	router2.ServeHTTP(recorder, writeRequest("PUT", "/dav/bench-folder/inner.bin", map[string]string{"Content-Type": "application/octet-stream"}, "x"))
	if recorder.Code != http.StatusLocked {
		t.Fatalf("locked put = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	router2.ServeHTTP(recorder, writeRequest("PUT", "/dav/bench-folder/inner.bin",
		map[string]string{"Content-Type": "application/octet-stream", "If": "<" + folderToken + ">"}, "x"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("token put = %d", recorder.Code)
	}
}

func TestDavUnlockSemantics(t *testing.T) {
	router, _ := newWriteRouter(t, map[string]model.RemoteEntry{"/bench-one.txt": fileEntryAt("bench-one.txt")}, nil, &fakeUploadStorage{})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("LOCK", "/dav/bench-one.txt", nil, lockInfoBody("")))
	token := lockTokenOf(recorder)

	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("UNLOCK", "/dav/bench-one.txt", map[string]string{"Lock-Token": "<opaquelocktoken:wrong>"}, ""))
	if recorder.Code != http.StatusConflict || recorder.Body.String() != "lock token is invalid\n" {
		t.Fatalf("wrong token = (%d, %q)", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("UNLOCK", "/dav/bench-one.txt", nil, ""))
	if recorder.Code != 400 || recorder.Body.String() != "Lock-Token header is required\n" {
		t.Fatalf("missing header = (%d, %q)", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("UNLOCK", "/dav/bench-one.txt", map[string]string{"Lock-Token": "<" + token + ">"}, ""))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("unlock = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("PUT", "/dav/bench-one.txt", map[string]string{"Content-Type": "text/plain"}, "after"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("put after unlock = %d", recorder.Code)
	}
}

// TestRestWriteRoutesLockChecks pins the REST write routes: folders,
// delete, rename/move, and the upload pair all answer with their JSON
// shapes and consult the shared lock store.
func TestRestWriteRoutesLockChecks(t *testing.T) {
	mutations := &recordingMutations{entry: model.RemoteEntry{ID: "e1", Name: "new", Kind: model.KindFolder, ParentID: model.Ptr("root")}}
	router, _ := newWriteRouter(t, map[string]model.RemoteEntry{"/src": fileEntryAt("src")}, mutations, nil)

	// POST folders answers {"path", "entry"}.
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("POST", "/api/v1/folders?path=%2Fnew-dir", nil, ""))
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `"path":"/new-dir"`) {
		t.Fatalf("folders = (%d, %q)", recorder.Code, recorder.Body.String())
	}

	// DELETE answers 204.
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("DELETE", "/api/v1/entries?path=%2Fsrc", nil, ""))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", recorder.Code)
	}

	// PATCH rename answers {"path", "entry"} with the new path.
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("PATCH", "/api/v1/entries?path=%2Fsrc", nil, `{"name":"renamed"}`))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"path":"/new"`) {
		t.Fatalf("rename = (%d, %q)", recorder.Code, recorder.Body.String())
	}
	if len(mutations.renames) != 1 || mutations.renames[0] != [2]string{"/src", "renamed"} {
		t.Fatalf("renames = %v", mutations.renames)
	}

	// A root lock with depth infinity blocks every descendant mutation;
	// each handler answers the JSON 423 framing and never reaches storage.
	router2, locks := newWriteRouter(t, map[string]model.RemoteEntry{"/src": fileEntryAt("src")}, mutations, nil)
	if _, err := locks.Acquire("/", "infinity", "owner", 600, ""); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{name: "folders", method: "POST", target: "/api/v1/folders?path=%2Fnew-dir"},
		{name: "delete", method: "DELETE", target: "/api/v1/entries?path=%2Fsrc"},
		{name: "put", method: "PUT", target: "/api/v1/files?path=%2Fsrc", body: "bytes"},
		{name: "rename", method: "PATCH", target: "/api/v1/entries?path=%2Fsrc", body: `{"name":"renamed"}`},
		{name: "move", method: "PATCH", target: "/api/v1/entries?path=%2Fsrc", body: `{"destination":"/other"}`},
		{name: "parent", method: "PATCH", target: "/api/v1/entries?path=%2Fsrc", body: `{"parent_path":"/other"}`},
	}
	before := len(mutations.folders) + len(mutations.deletes) + len(mutations.renames) + len(mutations.moves) + len(mutations.copies)
	for _, tc := range cases {
		recorder = httptest.NewRecorder()
		router2.ServeHTTP(recorder, writeRequest(tc.method, tc.target, nil, tc.body))
		if recorder.Code != http.StatusLocked {
			t.Fatalf("%s locked status = %d (body %q)", tc.name, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), `"error":"resource is locked"`) {
			t.Fatalf("%s locked body = %q", tc.name, recorder.Body.String())
		}
	}
	after := len(mutations.folders) + len(mutations.deletes) + len(mutations.renames) + len(mutations.moves) + len(mutations.copies)
	// Only the three unlocked calls from the first router may have landed.
	if after != before {
		t.Fatalf("locked mutations reached storage: %d -> %d", before, after)
	}
}

// TestRestPatchParentPathLocksExactChild pins the parent_path lock surface:
// both the parent and the exact child path are checked, using the source
// entry's metadata.
func TestRestPatchParentPathLocksExactChild(t *testing.T) {
	mutations := &recordingMutations{}
	router, locks := newWriteRouter(t, map[string]model.RemoteEntry{"/src": fileEntryAt("src")}, mutations, nil)

	// A depth 0 lock on the exact destination child blocks the move even
	// though the parent itself is free.
	if _, err := locks.Acquire("/other/src", "0", "owner", 600, ""); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, writeRequest("PATCH", "/api/v1/entries?path=%2Fsrc", nil, `{"parent_path":"/other"}`))
	if recorder.Code != http.StatusLocked {
		t.Fatalf("exact child lock status = %d", recorder.Code)
	}

	// A rename targeting the locked name is blocked the same way.
	router2, locks2 := newWriteRouter(t, map[string]model.RemoteEntry{"/src": fileEntryAt("src")}, mutations, nil)
	if _, err := locks2.Acquire("/renamed", "0", "owner", 600, ""); err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	router2.ServeHTTP(recorder, writeRequest("PATCH", "/api/v1/entries?path=%2Fsrc", nil, `{"fname":"renamed"}`))
	if recorder.Code != http.StatusLocked {
		t.Fatalf("rename lock status = %d", recorder.Code)
	}
	if len(mutations.renames) != 0 || len(mutations.moves) != 0 {
		t.Fatal("locked patches must not reach storage")
	}
}

// TestRestPatchValidationErrors pins the JSON-shape errors of the
// rename/move route.
func TestRestPatchValidationErrors(t *testing.T) {
	mutations := &recordingMutations{}
	router, _ := newWriteRouter(t, map[string]model.RemoteEntry{"/src": fileEntryAt("src")}, mutations, nil)
	cases := map[string]struct {
		payload string
		message string
	}{
		"both":      {`{"name":"a","destination":"/b"}`, "choose either a new name or a move destination"},
		"two names": {`{"name":"a","fname":"b"}`, "request contains multiple mutation targets"},
		"name type": {`{"name":3}`, "JSON field 'name' is required"},
		"dest type": {`{"destination":3}`, "JSON field 'destination' must be a path"},
		"null dest": {`{"destination":null}`, "JSON field 'destination' must be a path"},
		"missing":   {`{"unrelated":1}`, "JSON field 'name', 'destination' or 'parent_path' is required"},
	}
	for label, tc := range cases {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, writeRequest("PATCH", "/api/v1/entries?path=%2Fsrc", nil, tc.payload))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), tc.message) {
			t.Fatalf("%s = (%d, %q), want 400 with %q", label, recorder.Code, recorder.Body.String(), tc.message)
		}
	}
}
