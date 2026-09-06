package httpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// The PROPFIND goldens below mirror the ElementTree serialization byte for
// byte (namespace redeclared per response chunk, "<D:tag />" empty
// elements, only &amp;/&lt;/&gt; inside text); the harness-shaped entries
// reproduce the contract records DAV-PROPFIND-001..009.

const (
	propfindMtime      = "1788268272"
	propfindHTTPDate   = "Tue, 01 Sep 2026 13:11:12 GMT"
	propfindRootPrefix = `<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`
)

func propfindFile(id, name, etag string) model.RemoteEntry {
	return model.RemoteEntry{
		ID: id, Name: name, Kind: model.KindFile, ParentID: model.Ptr("0"),
		Size: model.Ptr(int64(11)), ModifiedAt: model.Ptr(propfindMtime), Etag: model.Ptr(etag),
	}
}

func propfindFolder(id, name, etag string) model.RemoteEntry {
	return model.RemoteEntry{
		ID: id, Name: name, Kind: model.KindFolder, ParentID: model.Ptr("0"),
		Size: model.Ptr(int64(11)), ModifiedAt: model.Ptr(propfindMtime), Etag: model.Ptr(etag),
	}
}

func propfindRootEntry() model.RemoteEntry {
	return model.RemoteEntry{ID: "0", Name: "WPS Enterprise Drive", Kind: model.KindFolder, Size: model.Ptr(int64(0))}
}

// propfindStorage serves a fixed root entry and children per path.
type propfindStorage struct {
	root       model.RemoteEntry
	byPath     map[string]model.RemoteEntry
	rootErr    error
	listByPath map[string][]model.RemoteEntry
	// childrenByEntry serves the B703 by-ID descent: children of deeper
	// folders are listed by parent entry ID, never re-resolved by path.
	childrenByEntry map[string][]model.RemoteEntry
	onChildren      func(entryID string)
	listErr         error
	listCalls       []string
}

func (f *propfindStorage) Metadata(path string) (model.RemoteEntry, error) {
	if f.rootErr != nil {
		return model.RemoteEntry{}, f.rootErr
	}
	if f.byPath != nil {
		if entry, ok := f.byPath[path]; ok {
			return entry, nil
		}
	}
	return f.root, nil
}

func (f *propfindStorage) ListPath(path string) ([]model.RemoteEntry, error) {
	f.listCalls = append(f.listCalls, path)
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listByPath != nil {
		if children, ok := f.listByPath[path]; ok {
			return children, nil
		}
	}
	return nil, nil
}

func (f *propfindStorage) ListChildren(scopePath string, entry model.RemoteEntry) ([]model.RemoteEntry, error) {
	f.listCalls = append(f.listCalls, "children:"+entry.ID)
	if f.onChildren != nil {
		f.onChildren(entry.ID)
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.childrenByEntry != nil {
		if children, ok := f.childrenByEntry[entry.ID]; ok {
			return children, nil
		}
	}
	return nil, nil
}

func newPropfindRouter(t *testing.T, storage *propfindStorage, mutate func(*DAVDispatcher)) *Router {
	t.Helper()
	limits := ControlLimits{MaxControlBody: 64 * 1024, MaxResponseBody: 16 * 1024 * 1024}
	dispatcher, err := NewDAVDispatcher(storage, limits, DAVLimits{MaxPropfindEntries: 10000, MaxPropfindDepth: 64}, "/dav")
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(dispatcher)
	}
	router, err := NewRouter(RouterConfig{
		DAVPrefix:  "/dav",
		RESTPrefix: "/api/v1",
		Handlers: Handlers{
			Health:   func(w http.ResponseWriter, r *http.Request) {},
			WebApp:   func(w http.ResponseWriter, r *http.Request) {},
			WebAsset: func(w http.ResponseWriter, r *http.Request, name string) {},
			REST: func(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
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

func servePropfind(t *testing.T, router *Router, target string, depth string) *httptest.ResponseRecorder {
	t.Helper()
	request := newTestRequest("PROPFIND", target)
	if depth != "" {
		request.Header.Set("Depth", depth)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

var hrefPattern = regexp.MustCompile(`<D:href>(.*?)</D:href>`)

func hrefs(body string) []string {
	found := hrefPattern.FindAllStringSubmatch(body, -1)
	result := make([]string, 0, len(found))
	for _, match := range found {
		result = append(result, match[1])
	}
	return result
}

// TestPropfindDepthZeroFile replays DAV-PROPFIND-001: a file answers one
// response without the collection element, with quoted etag and the
// formatted HTTP date.
func TestPropfindDepthZeroFile(t *testing.T) {
	storage := &propfindStorage{root: propfindFile("bench-file-1", "bench-one.txt", "bench-etag-bench-file-1")}
	recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/bench-one.txt", "0")
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	want := propfindRootPrefix +
		`<D:response xmlns:D="DAV:"><D:href>/dav/bench-one.txt</D:href><D:propstat><D:prop><D:resourcetype /><D:displayname>bench-one.txt</D:displayname><D:getcontentlength>11</D:getcontentlength><D:getcontenttype>text/plain</D:getcontenttype><D:getetag>"bench-etag-bench-file-1"</D:getetag><D:getlastmodified>Tue, 01 Sep 2026 13:11:12 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`</D:multistatus>`
	if body := recorder.Body.String(); body != want {
		t.Errorf("body =\n%q\nwant\n%q", body, want)
	}
	if ct := recorder.Header().Get("Content-Type"); ct != propfindContentType {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := recorder.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if dav := recorder.Header()["DAV"]; len(dav) != 1 || dav[0] != "1,2" {
		t.Errorf("DAV = %q", dav)
	}
}

// TestPropfindDepthZeroDirectory keeps the collection element and omits
// etag and lastmodified when the entry lacks them.
func TestPropfindDepthZeroDirectory(t *testing.T) {
	storage := &propfindStorage{root: propfindRootEntry()}
	recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/", "0")
	want := propfindRootPrefix +
		`<D:response xmlns:D="DAV:"><D:href>/dav/</D:href><D:propstat><D:prop><D:resourcetype><D:collection /></D:resourcetype><D:displayname>WPS Enterprise Drive</D:displayname><D:getcontentlength>0</D:getcontentlength><D:getcontenttype>httpd/unix-directory</D:getcontenttype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`</D:multistatus>`
	if recorder.Code != http.StatusMultiStatus || recorder.Body.String() != want {
		t.Fatalf("status = %d body =\n%q", recorder.Code, recorder.Body.String())
	}
	if len(storage.listCalls) != 0 {
		t.Errorf("Depth 0 must not list: %v", storage.listCalls)
	}
}

// TestPropfindDepthOneRoot replays DAV-PROPFIND-002: the root first, then
// the direct children in listing order, folders with a trailing slash.
func TestPropfindDepthOneRoot(t *testing.T) {
	storage := &propfindStorage{
		root: propfindRootEntry(),
		listByPath: map[string][]model.RemoteEntry{
			"/": {
				propfindFile("bench-file-1", "bench-one.txt", "bench-etag-bench-file-1"),
				propfindFile("bench-file-2", "bench-two.txt", "bench-etag-bench-file-2"),
				propfindFolder("bench-dir-1", "bench-folder", "bench-etag-bench-dir-1"),
			},
		},
	}
	recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/", "1")
	want := propfindRootPrefix +
		`<D:response xmlns:D="DAV:"><D:href>/dav/</D:href><D:propstat><D:prop><D:resourcetype><D:collection /></D:resourcetype><D:displayname>WPS Enterprise Drive</D:displayname><D:getcontentlength>0</D:getcontentlength><D:getcontenttype>httpd/unix-directory</D:getcontenttype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`<D:response xmlns:D="DAV:"><D:href>/dav/bench-one.txt</D:href><D:propstat><D:prop><D:resourcetype /><D:displayname>bench-one.txt</D:displayname><D:getcontentlength>11</D:getcontentlength><D:getcontenttype>text/plain</D:getcontenttype><D:getetag>"bench-etag-bench-file-1"</D:getetag><D:getlastmodified>Tue, 01 Sep 2026 13:11:12 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`<D:response xmlns:D="DAV:"><D:href>/dav/bench-two.txt</D:href><D:propstat><D:prop><D:resourcetype /><D:displayname>bench-two.txt</D:displayname><D:getcontentlength>11</D:getcontentlength><D:getcontenttype>text/plain</D:getcontenttype><D:getetag>"bench-etag-bench-file-2"</D:getetag><D:getlastmodified>Tue, 01 Sep 2026 13:11:12 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`<D:response xmlns:D="DAV:"><D:href>/dav/bench-folder/</D:href><D:propstat><D:prop><D:resourcetype><D:collection /></D:resourcetype><D:displayname>bench-folder</D:displayname><D:getcontentlength>11</D:getcontentlength><D:getcontenttype>httpd/unix-directory</D:getcontenttype><D:getetag>"bench-etag-bench-dir-1"</D:getetag><D:getlastmodified>Tue, 01 Sep 2026 13:11:12 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`</D:multistatus>`
	if recorder.Code != http.StatusMultiStatus || recorder.Body.String() != want {
		t.Fatalf("status = %d body =\n%q", recorder.Code, recorder.Body.String())
	}
	if len(storage.listCalls) != 1 || storage.listCalls[0] != "/" {
		t.Errorf("Depth 1 lists only the root: %v", storage.listCalls)
	}
}

// TestPropfindDepthDefaultsToOne replays DAV-PROPFIND-005: a missing Depth
// header behaves exactly like Depth 1.
func TestPropfindDepthDefaultsToOne(t *testing.T) {
	newStorage := func() *propfindStorage {
		return &propfindStorage{
			root: propfindRootEntry(),
			listByPath: map[string][]model.RemoteEntry{
				"/": {propfindFile("bench-file-1", "bench-one.txt", "bench-etag-bench-file-1")},
			},
		}
	}
	explicit := servePropfind(t, newPropfindRouter(t, newStorage(), nil), "/dav/", "1")
	implicit := servePropfind(t, newPropfindRouter(t, newStorage(), nil), "/dav/", "")
	if explicit.Code != implicit.Code || explicit.Body.String() != implicit.Body.String() {
		t.Fatalf("default Depth differs: %d %q vs %d %q", explicit.Code, explicit.Body.String(), implicit.Code, implicit.Body.String())
	}
}

// TestPropfindDepthOneSubfolder replays DAV-PROPFIND-003: the folder is the
// first answer, its children follow, and nothing deeper is listed.
func TestPropfindDepthOneSubfolder(t *testing.T) {
	storage := &propfindStorage{
		root: propfindRootEntry(),
		byPath: map[string]model.RemoteEntry{
			// The metadata call keeps the request's trailing slash while
			// the joined list_path call does not (normpath parity).
			"/bench-folder/": propfindFolder("bench-dir-1", "bench-folder", "bench-etag-bench-dir-1"),
		},
		listByPath: map[string][]model.RemoteEntry{
			"/bench-folder/": {propfindFile("child-1", "child.txt", "bench-etag-child")},
		},
	}
	recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/bench-folder/", "1")
	want := propfindRootPrefix +
		`<D:response xmlns:D="DAV:"><D:href>/dav/bench-folder/</D:href><D:propstat><D:prop><D:resourcetype><D:collection /></D:resourcetype><D:displayname>bench-folder</D:displayname><D:getcontentlength>11</D:getcontentlength><D:getcontenttype>httpd/unix-directory</D:getcontenttype><D:getetag>"bench-etag-bench-dir-1"</D:getetag><D:getlastmodified>Tue, 01 Sep 2026 13:11:12 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`<D:response xmlns:D="DAV:"><D:href>/dav/bench-folder/child.txt</D:href><D:propstat><D:prop><D:resourcetype /><D:displayname>child.txt</D:displayname><D:getcontentlength>11</D:getcontentlength><D:getcontenttype>text/plain</D:getcontenttype><D:getetag>"bench-etag-child"</D:getetag><D:getlastmodified>Tue, 01 Sep 2026 13:11:12 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`</D:multistatus>`
	if recorder.Code != http.StatusMultiStatus || recorder.Body.String() != want {
		t.Fatalf("status = %d body =\n%q", recorder.Code, recorder.Body.String())
	}
	if len(storage.listCalls) != 1 || storage.listCalls[0] != "/bench-folder/" {
		t.Errorf("list calls = %v", storage.listCalls)
	}
}

// TestPropfindDepthValidation replays DAV-PROPFIND-006: out-of-range depths
// answer 400, and the header is stripped and lowercased first.
func TestPropfindDepthValidation(t *testing.T) {
	storage := &propfindStorage{root: propfindRootEntry()}
	router := newPropfindRouter(t, storage, nil)
	recorder := servePropfind(t, router, "/dav/", "2")
	if recorder.Code != http.StatusBadRequest || recorder.Body.String() != "Depth must be 0, 1 or infinity\n" {
		t.Fatalf("Depth 2: status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	// An explicitly empty Depth header is not the missing default.
	empty := newTestRequest("PROPFIND", "/dav/")
	empty.Header.Set("Depth", "")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, empty)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty Depth: status = %d", recorder.Code)
	}
	recorder = servePropfind(t, router, "/dav/", "INFINITY")
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("INFINITY: status = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	recorder = servePropfind(t, router, "/dav/", " 0 ")
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("padded Depth: status = %d", recorder.Code)
	}
}

// TestPropfindContractHrefs replays the href lists of records
// DAV-PROPFIND-002/003/004/005 against the same walk.
func TestPropfindContractHrefs(t *testing.T) {
	record := loadRecord(t, "DAV-PROPFIND-002")
	storage := &propfindStorage{
		root: propfindRootEntry(),
		listByPath: map[string][]model.RemoteEntry{
			"/": {
				propfindFile("bench-file-1", "bench-one.txt", "bench-etag-bench-file-1"),
				propfindFile("bench-file-2", "bench-two.txt", "bench-etag-bench-file-2"),
				propfindFolder("bench-dir-1", "bench-folder", "bench-etag-bench-dir-1"),
			},
		},
	}
	recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/", "1")
	if recorder.Code != int(record["status"].(float64)) {
		t.Fatalf("status = %d", recorder.Code)
	}
	want := record["hrefs"].([]any)
	got := hrefs(recorder.Body.String())
	if len(got) != len(want) {
		t.Fatalf("hrefs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i].(string) {
			t.Errorf("href[%d] = %q, want %q", i, got[i], want[i].(string))
		}
	}

	record = loadRecord(t, "DAV-PROPFIND-003")
	storage = &propfindStorage{
		root: propfindRootEntry(),
		byPath: map[string]model.RemoteEntry{
			"/bench-folder/": propfindFolder("bench-dir-1", "bench-folder", "bench-etag-bench-dir-1"),
		},
		listByPath: map[string][]model.RemoteEntry{
			"/":              {propfindFolder("bench-dir-1", "bench-folder", "bench-etag-bench-dir-1")},
			"/bench-folder/": {propfindFile("child-1", "child.txt", "bench-etag-child")},
		},
	}
	recorder = servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/bench-folder/", "1")
	got = hrefs(recorder.Body.String())
	want = record["hrefs"].([]any)
	if len(got) != len(want) {
		t.Fatalf("subfolder hrefs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i].(string) {
			t.Errorf("subfolder href[%d] = %q, want %q", i, got[i], want[i].(string))
		}
	}

	// DAV-PROPFIND-004 (Depth infinity on a bounded tree) and 005 (missing
	// Depth): the walk answers both; B703 still owns the queue-based
	// hardening for deep trees.
	record4 := loadRecord(t, "DAV-PROPFIND-004")
	storage = &propfindStorage{
		root: propfindRootEntry(),
		listByPath: map[string][]model.RemoteEntry{
			"/": {
				propfindFile("bench-file-1", "bench-one.txt", "bench-etag-bench-file-1"),
				propfindFile("bench-file-2", "bench-two.txt", "bench-etag-bench-file-2"),
				propfindFolder("bench-dir-1", "bench-folder", "bench-etag-bench-dir-1"),
			},
		},
		childrenByEntry: map[string][]model.RemoteEntry{
			"bench-dir-1": {propfindFile("child-1", "child.txt", "bench-etag-child")},
		},
	}
	recorder = servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/", "infinity")
	got = hrefs(recorder.Body.String())
	want = record4["hrefs"].([]any)
	if len(got) != len(want) {
		t.Fatalf("infinity hrefs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i].(string) {
			t.Errorf("infinity href[%d] = %q, want %q", i, got[i], want[i].(string))
		}
	}
	record5 := loadRecord(t, "DAV-PROPFIND-005")
	if len(hrefs(implicitBody(t, storage))) != int(record5["href_count"].(float64)) {
		t.Fatalf("default-depth href count mismatch")
	}
}

func implicitBody(t *testing.T, storage *propfindStorage) string {
	t.Helper()
	return servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/", "").Body.String()
}

// TestPropfindSpecialCharacters replays DAV-PROPFIND-009: href components
// are percent-encoded per segment, the displayname keeps raw quotes and
// escapes only &, <, >, and non-ASCII names stay raw UTF-8 in text while
// the href gains uppercase percent escapes.
func TestPropfindSpecialCharacters(t *testing.T) {
	tricky := propfindFile("bench-tricky", `a&b<c>"d'.txt`, "bench-etag-bench-tricky")
	storage := &propfindStorage{
		root: propfindRootEntry(),
		listByPath: map[string][]model.RemoteEntry{
			"/": {tricky},
		},
	}
	recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/", "1")
	want := propfindRootPrefix +
		`<D:response xmlns:D="DAV:"><D:href>/dav/</D:href><D:propstat><D:prop><D:resourcetype><D:collection /></D:resourcetype><D:displayname>WPS Enterprise Drive</D:displayname><D:getcontentlength>0</D:getcontentlength><D:getcontenttype>httpd/unix-directory</D:getcontenttype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`<D:response xmlns:D="DAV:"><D:href>/dav/a%26b%3Cc%3E%22d%27.txt</D:href><D:propstat><D:prop><D:resourcetype /><D:displayname>a&amp;b&lt;c&gt;"d'.txt</D:displayname><D:getcontentlength>11</D:getcontentlength><D:getcontenttype>text/plain</D:getcontenttype><D:getetag>"bench-etag-bench-tricky"</D:getetag><D:getlastmodified>Tue, 01 Sep 2026 13:11:12 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`</D:multistatus>`
	if recorder.Code != http.StatusMultiStatus || recorder.Body.String() != want {
		t.Fatalf("status = %d body =\n%q", recorder.Code, recorder.Body.String())
	}
}

// TestPropfindUnicodeName pins the UTF-8 behavior: the displayname carries
// raw UTF-8 while the href percent-encodes every byte uppercase.
func TestPropfindUnicodeName(t *testing.T) {
	storage := &propfindStorage{
		root: propfindRootEntry(),
		listByPath: map[string][]model.RemoteEntry{
			"/": {propfindFile("unicode-1", "报表.txt", "etag-unicode")},
		},
	}
	recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/", "1")
	body := recorder.Body.String()
	if !strings.Contains(body, "<D:displayname>报表.txt</D:displayname>") {
		t.Errorf("raw UTF-8 displayname missing: %q", body)
	}
	if !strings.Contains(body, "/dav/%E6%8A%A5%E8%A1%A8.txt") {
		t.Errorf("percent-encoded href missing: %q", body)
	}
}

// TestPropfindErrors replays DAV-PROPFIND-007/008 plus upstream mapping.
func TestPropfindErrors(t *testing.T) {
	storage := &propfindStorage{rootErr: model.NewStorageError(model.KindEntryNotFound, "entry not found: missing.txt")}
	router := newPropfindRouter(t, storage, nil)
	recorder := servePropfind(t, router, "/dav/missing.txt", "0")
	if recorder.Code != http.StatusNotFound || recorder.Body.String() != "entry not found: missing.txt\n" {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	recorder = servePropfind(t, router, "/not-a-route", "0")
	if recorder.Code != http.StatusNotFound || recorder.Body.String() != "unknown route\n" {
		t.Fatalf("outside prefix: status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	upstream := &propfindStorage{rootErr: model.NewWpsAPIError("list entries", 500, model.WpsCategoryUpstream)}
	recorder = servePropfind(t, newPropfindRouter(t, upstream, nil), "/dav/", "0")
	if recorder.Code != http.StatusBadGateway || recorder.Body.String() != "upstream WPS request failed\n" {
		t.Fatalf("upstream: status = %d body = %q", recorder.Code, recorder.Body.String())
	}
}

// TestPropfindLimits covers the three 507 bounds and the repeated-ID
// integrity failure.
func TestPropfindLimits(t *testing.T) {
	children := func() []model.RemoteEntry {
		return []model.RemoteEntry{propfindFile("f1", "one.txt", "e1"), propfindFile("f2", "two.txt", "e2")}
	}
	t.Run("entry limit", func(t *testing.T) {
		storage := &propfindStorage{root: propfindRootEntry(), listByPath: map[string][]model.RemoteEntry{"/": children()}}
		router := newPropfindRouter(t, storage, func(d *DAVDispatcher) { d.propfind.MaxPropfindEntries = 2 })
		recorder := servePropfind(t, router, "/dav/", "1")
		if recorder.Code != http.StatusInsufficientStorage || recorder.Body.String() != "PROPFIND exceeds the configured entry limit\n" {
			t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("depth limit", func(t *testing.T) {
		storage := &propfindStorage{
			root: propfindRootEntry(),
			listByPath: map[string][]model.RemoteEntry{
				"/":             {propfindFolder("dir", "bench-folder", "e1")},
				"/bench-folder": {propfindFile("f1", "one.txt", "e2")},
			},
		}
		router := newPropfindRouter(t, storage, func(d *DAVDispatcher) { d.propfind.MaxPropfindDepth = 1 })
		recorder := servePropfind(t, router, "/dav/", "infinity")
		if recorder.Code != http.StatusInsufficientStorage || recorder.Body.String() != "PROPFIND exceeds the configured depth limit\n" {
			t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("response size", func(t *testing.T) {
		storage := &propfindStorage{root: propfindRootEntry(), listByPath: map[string][]model.RemoteEntry{"/": children()}}
		router := newPropfindRouter(t, storage, func(d *DAVDispatcher) { d.limits.MaxResponseBody = 150 })
		recorder := servePropfind(t, router, "/dav/", "1")
		if recorder.Code != http.StatusInsufficientStorage || recorder.Body.String() != "PROPFIND response exceeds the configured size limit\n" {
			t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("repeated entry id", func(t *testing.T) {
		child := propfindFile("repeated", "one.txt", "e1")
		storage := &propfindStorage{
			root:       model.RemoteEntry{ID: "repeated", Name: "WPS Enterprise Drive", Kind: model.KindFolder, Size: model.Ptr(int64(0))},
			listByPath: map[string][]model.RemoteEntry{"/": {child}},
		}
		recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/", "1")
		// The repeated ID is an upstream integrity failure: fixed 502 text,
		// the detail never reaches the client.
		if recorder.Code != http.StatusBadGateway || recorder.Body.String() != "upstream WPS request failed\n" {
			t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
		}
	})
}

// TestPropfindDiscardsBody pins the bounded discard: any request body (a
// prop selection or garbage) is ignored, and an oversized declared body is
// refused with the empty 413 before reading.
func TestPropfindDiscardsBody(t *testing.T) {
	storage := &propfindStorage{
		root:       propfindRootEntry(),
		listByPath: map[string][]model.RemoteEntry{"/": {propfindFile("f1", "one.txt", "e1")}},
	}
	router := newPropfindRouter(t, storage, nil)
	recorder := httptest.NewRecorder()
	request := newTestRequest("PROPFIND", "/dav/")
	body := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:getetag /></D:prop></D:propfind>`
	request.Body = io.NopCloser(strings.NewReader(body))
	request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("propfind with body: status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "<D:getetag>") {
		t.Errorf("prop selection must be ignored: %q", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = newTestRequest("PROPFIND", "/dav/")
	request.Header.Set("Content-Length", "999999")
	router.ServeHTTP(recorder, request)
	// The empty message keeps the text framing: a bare newline body.
	if recorder.Code != http.StatusRequestEntityTooLarge || recorder.Body.String() != "\n" {
		t.Fatalf("oversized body: status = %d body = %q", recorder.Code, recorder.Body.String())
	}
}

// TestPythonQuote pins quote(part, safe="") semantics.
func TestPythonQuote(t *testing.T) {
	cases := map[string]string{
		"bench-one.txt":  "bench-one.txt",
		"a&b<c>\"d'.txt": "a%26b%3Cc%3E%22d%27.txt",
		"报表.txt":         "%E6%8A%A5%E8%A1%A8.txt",
		"space name":     "space%20name",
		"tilde~_dash-.":  "tilde~_dash-.",
		"percent%":       "percent%25",
		"plus+":          "plus%2B",
		"colon:":         "colon%3A",
	}
	for input, want := range cases {
		if got := pythonQuote(input); got != want {
			t.Errorf("pythonQuote(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestHTTPDate pins formatdate(usegmt=True) parity and the None fallbacks.
func TestHTTPDate(t *testing.T) {
	valid := "1788268272"
	if got := httpDate(&valid); got != "Tue, 01 Sep 2026 13:11:12 GMT" {
		t.Errorf("httpDate(epoch) = %q", got)
	}
	fraction := "1788268272.9"
	if got := httpDate(&fraction); got != "Tue, 01 Sep 2026 13:11:12 GMT" {
		t.Errorf("httpDate(fraction) = %q", got)
	}
	negative := "-1"
	if got := httpDate(&negative); got != "Wed, 31 Dec 1969 23:59:59 GMT" {
		t.Errorf("httpDate(negative) = %q", got)
	}
	padded := " 1788268272 "
	if got := httpDate(&padded); got != "Tue, 01 Sep 2026 13:11:12 GMT" {
		t.Errorf("httpDate(padded) = %q", got)
	}
	for _, invalid := range []string{"", "abc", "nan", "inf", "1e30"} {
		if got := httpDate(&invalid); got != "" {
			t.Errorf("httpDate(%q) = %q, want empty", invalid, got)
		}
	}
	if got := httpDate(nil); got != "" {
		t.Errorf("httpDate(nil) = %q", got)
	}
}

// TestPropfindLiveWire probes the real transport (the curl view): 207 with
// the raw-case DAV capability header, the XML content type, and a body.
func TestPropfindLiveWire(t *testing.T) {
	storage := &propfindStorage{
		root: propfindRootEntry(),
		listByPath: map[string][]model.RemoteEntry{
			"/": {propfindFile("bench-file-1", "bench-one.txt", "bench-etag-bench-file-1")},
		},
	}
	router := newPropfindRouter(t, storage, nil)
	server := httptest.NewServer(router)
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")

	response := rawExchange(t, address, "PROPFIND /dav/ HTTP/1.1\r\nHost: x\r\nDepth: 1\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 207") {
		t.Fatalf("propfind status: %s", strings.SplitN(response, "\r\n", 2)[0])
	}
	if !strings.Contains(response, "DAV: 1,2\r\n") {
		t.Errorf("raw-case DAV header missing: %s", response)
	}
	if !strings.Contains(response, "Content-Type: application/xml; charset=utf-8\r\n") {
		t.Errorf("XML content type missing: %s", response)
	}
	if !strings.Contains(response, "<D:multistatus") {
		t.Errorf("multistatus body missing: %s", response)
	}
}
