package httpserver

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// fakeReadStorage stands in for the read-only storage surface behind the
// REST GET routes; every call is recorded so tests can assert dispatch
// order and the exact path handed to the storage.
type fakeReadStorage struct {
	metadataEntry model.RemoteEntry
	metadataErr   error
	entries       []model.RemoteEntry
	listErr       error
	calls         []string
}

func (f *fakeReadStorage) Metadata(path string) (model.RemoteEntry, error) {
	f.calls = append(f.calls, "metadata:"+path)
	if f.metadataErr != nil {
		return model.RemoteEntry{}, f.metadataErr
	}
	return f.metadataEntry, nil
}

func (f *fakeReadStorage) ListPath(path string) ([]model.RemoteEntry, error) {
	f.calls = append(f.calls, "list:"+path)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.entries, nil
}

type fakeStatusRoots struct {
	rootID string
	err    error
}

func (f *fakeStatusRoots) StatusRootID() (string, error) { return f.rootID, f.err }

type fakeStatusChecker struct {
	status     model.WpsStatus
	err        error
	seenRootID string
}

func (f *fakeStatusChecker) CheckStatus(rootID string) (model.WpsStatus, error) {
	f.seenRootID = rootID
	return f.status, f.err
}

// newReadDispatcher builds a dispatcher around the given read surface; the
// session importer is inert because the read routes never touch it.
func newReadDispatcher(t *testing.T, read RESTReadStorage, status *StatusController) *RESTDispatcher {
	t.Helper()
	return newReadDispatcherDownloads(t, read, status, stubDownloadStorage{})
}

func newReadDispatcherDownloads(t *testing.T, read RESTReadStorage, status *StatusController, downloads DownloadStorage) *RESTDispatcher {
	t.Helper()
	limits := ControlLimits{MaxControlBody: 64 * 1024, MaxResponseBody: 64 * 1024}
	session, err := NewSessionImporter(limits, "",
		func([]any, string) (string, string, []string, error) {
			return "", "", nil, errBadRequest("unused in read tests")
		},
		&recordingCredentialReplacer{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewRootNameController(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewRESTDispatcher(limits, controller, session, read, status, DownloadLimits{}, downloads, stubUploadStorage{}, stubMutations{}, newTestLockStore(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher
}

func readRouter(t *testing.T, dispatcher *RESTDispatcher) *Router {
	t.Helper()
	router, err := NewRouter(RouterConfig{
		DAVPrefix:  "/dav",
		RESTPrefix: "/api/v1",
		Handlers: Handlers{
			Health:   func(w http.ResponseWriter, r *http.Request) {},
			WebApp:   func(w http.ResponseWriter, r *http.Request) {},
			WebAsset: func(w http.ResponseWriter, r *http.Request, name string) {},
			REST:     dispatcher.ServeREST,
			DAV: func(w http.ResponseWriter, r *http.Request, davPath string) error {
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func folderEntry(id string) model.RemoteEntry {
	return model.RemoteEntry{ID: id, Name: "bench-folder", Kind: model.KindFolder, Size: model.Ptr(int64(0))}
}

func fileEntry(id, name string) model.RemoteEntry {
	return model.RemoteEntry{
		ID:         id,
		Name:       name,
		Kind:       model.KindFile,
		ParentID:   model.Ptr("root-1"),
		Size:       model.Ptr(int64(11)),
		ModifiedAt: model.Ptr("2026-01-01T00:00:00Z"),
		Etag:       model.Ptr("etag-1"),
	}
}

func nullFileEntry(id, name string) model.RemoteEntry {
	return model.RemoteEntry{ID: id, Name: name, Kind: model.KindFile}
}

// TestRESTStatusRoute covers the status route: the redacted preflight, the
// no-checker fallback, shape failures, API-error propagation, and the
// query/body tolerance Python grants the route before _query_path runs.
func TestRESTStatusRoute(t *testing.T) {
	t.Run("success echoes the redacted preflight", func(t *testing.T) {
		roots := &fakeStatusRoots{rootID: "root-1"}
		checker := &fakeStatusChecker{status: model.WpsStatus{
			Status: "connected", Wps: "connected", Workspace: "ready",
			AccountType: "business", LastCheckedAt: model.Ptr(1788654409),
		}}
		dispatcher := newReadDispatcher(t, &fakeReadStorage{}, NewStatusController(roots, checker))
		router := readRouter(t, dispatcher)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/status"))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
		}
		want := `{"status":"connected","wps":"connected","workspace":"ready","account_type":"business","last_checked_at":1788654409,"retry_after":0}`
		if body := recorder.Body.String(); body != want {
			t.Errorf("body = %q, want %q", body, want)
		}
		if checker.seenRootID != "root-1" {
			t.Errorf("checker root id = %q, want %q", checker.seenRootID, "root-1")
		}
	})
	t.Run("without a checker answers not_configured", func(t *testing.T) {
		status := NewStatusController(nil, nil)
		status.now = func() int { return 1234 }
		dispatcher := newReadDispatcher(t, &fakeReadStorage{}, status)
		router := readRouter(t, dispatcher)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/status"))
		want := `{"status":"not_configured","wps":"not_configured","workspace":"not_configured","account_type":"unknown","last_checked_at":1234,"retry_after":0}`
		if recorder.Code != http.StatusOK || recorder.Body.String() != want {
			t.Fatalf("status = %d body = %q, want 200 %q", recorder.Code, recorder.Body.String(), want)
		}
	})
	t.Run("shape failures answer invalid_response", func(t *testing.T) {
		cases := map[string]*StatusController{
			"probe error":       withNow(NewStatusController(&fakeStatusRoots{rootID: "r"}, &fakeStatusChecker{err: errors.New("boom")})),
			"root error":        withNow(NewStatusController(&fakeStatusRoots{err: errors.New("io")}, &fakeStatusChecker{})),
			"root unresolvable": withNow(NewStatusController(nil, &fakeStatusChecker{})),
		}
		for name, status := range cases {
			t.Run(name, func(t *testing.T) {
				status.now = func() int { return 4321 }
				dispatcher := newReadDispatcher(t, &fakeReadStorage{}, status)
				router := readRouter(t, dispatcher)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/status"))
				want := `{"status":"invalid_response","wps":"unknown","workspace":"unknown","account_type":"unknown","last_checked_at":4321,"retry_after":0}`
				if recorder.Code != http.StatusOK || recorder.Body.String() != want {
					t.Fatalf("status = %d body = %q, want 200 %q", recorder.Code, recorder.Body.String(), want)
				}
			})
		}
	})
	t.Run("api errors propagate to the error table", func(t *testing.T) {
		checker := &fakeStatusChecker{err: model.NewWpsAPIError("probe failed", 500, model.WpsCategoryUpstream)}
		dispatcher := newReadDispatcher(t, &fakeReadStorage{}, NewStatusController(&fakeStatusRoots{rootID: "r"}, checker))
		router := readRouter(t, dispatcher)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/status"))
		want := `{"error":"upstream WPS request failed","code":"wps_unavailable","upstream_status":500}`
		if recorder.Code != http.StatusBadGateway || recorder.Body.String() != want {
			t.Fatalf("status = %d body = %q, want 502 %q", recorder.Code, recorder.Body.String(), want)
		}
	})
	t.Run("path query is ignored before validation", func(t *testing.T) {
		// Python returns for status before _query_path, so even a relative
		// path parameter cannot break the route.
		dispatcher := newReadDispatcher(t, &fakeReadStorage{}, NewStatusController(nil, nil))
		router := readRouter(t, dispatcher)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/status?path=relative"))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("request body is discarded", func(t *testing.T) {
		dispatcher := newReadDispatcher(t, &fakeReadStorage{}, NewStatusController(nil, nil))
		router := readRouter(t, dispatcher)
		request := httptest.NewRequest("GET", "/api/v1/status", strings.NewReader("hello"))
		request.Header.Set("Content-Length", "5")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
		}
	})
}

func withNow(c *StatusController) *StatusController {
	c.now = func() int { return 4321 }
	return c
}

// TestRESTEntriesRoute covers entries/list: success with null and unicode
// parity, the file conflict, not found, invalid paths, upstream errors, and
// the raw path echo.
func TestRESTEntriesRoute(t *testing.T) {
	newStorage := func() *fakeReadStorage {
		return &fakeReadStorage{
			metadataEntry: folderEntry("root-1"),
			entries: []model.RemoteEntry{
				fileEntry("id-1", "bench-one.txt"),
				folderEntry("id-2"),
				nullFileEntry("id-3", "报表.txt"),
			},
		}
	}
	t.Run("success keeps order, nulls, and ensure_ascii", func(t *testing.T) {
		storage := newStorage()
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/entries?path=%2F"))
		want := `{"path":"/","entries":[` +
			`{"id":"id-1","name":"bench-one.txt","kind":"file","parent_id":"root-1","size":11,"modified_at":"2026-01-01T00:00:00Z","etag":"etag-1"},` +
			`{"id":"id-2","name":"bench-folder","kind":"folder","parent_id":null,"size":0,"modified_at":null,"etag":null},` +
			`{"id":"id-3","name":"\u62a5\u8868.txt","kind":"file","parent_id":null,"size":null,"modified_at":null,"etag":null}` +
			`]}`
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
		}
		if body := recorder.Body.String(); body != want {
			t.Errorf("body = %q, want %q", body, want)
		}
		if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
		if len(storage.calls) != 2 || storage.calls[0] != "metadata:/" || storage.calls[1] != "list:/" {
			t.Errorf("storage calls = %v", storage.calls)
		}
	})
	t.Run("empty folder serializes an empty array", func(t *testing.T) {
		storage := &fakeReadStorage{metadataEntry: folderEntry("root-1"), entries: []model.RemoteEntry{}}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/entries?path=%2F"))
		want := `{"path":"/","entries":[]}`
		if recorder.Code != http.StatusOK || recorder.Body.String() != want {
			t.Fatalf("status = %d body = %q, want 200 %q", recorder.Code, recorder.Body.String(), want)
		}
	})
	t.Run("list alias returns identical bytes", func(t *testing.T) {
		first := httptest.NewRecorder()
		readRouter(t, newReadDispatcher(t, newStorage(), nil)).ServeHTTP(first, newTestRequest("GET", "/api/v1/entries?path=%2F"))
		second := httptest.NewRecorder()
		readRouter(t, newReadDispatcher(t, newStorage(), nil)).ServeHTTP(second, newTestRequest("GET", "/api/v1/list?path=%2F"))
		if first.Code != second.Code || first.Body.String() != second.Body.String() {
			t.Fatalf("entries/list mismatch: %d %q vs %d %q", first.Code, first.Body.String(), second.Code, second.Body.String())
		}
	})
	t.Run("entries on a file conflicts", func(t *testing.T) {
		storage := &fakeReadStorage{metadataEntry: fileEntry("id-1", "bench-one.txt")}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/entries?path=%2Fbench-one.txt"))
		want := `{"error":"the requested path is not a folder"}`
		if recorder.Code != http.StatusConflict || recorder.Body.String() != want {
			t.Fatalf("status = %d body = %q, want 409 %q", recorder.Code, recorder.Body.String(), want)
		}
		for _, call := range storage.calls {
			if strings.HasPrefix(call, "list:") {
				t.Errorf("list_path was called after the conflict: %v", storage.calls)
			}
		}
	})
	t.Run("missing path answers entry not found", func(t *testing.T) {
		storage := &fakeReadStorage{metadataErr: model.NewStorageError(model.KindEntryNotFound, "entry not found: missing")}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/entries?path=%2Fmissing"))
		want := `{"error":"entry not found: missing"}`
		if recorder.Code != http.StatusNotFound || recorder.Body.String() != want {
			t.Fatalf("status = %d body = %q, want 404 %q", recorder.Code, recorder.Body.String(), want)
		}
	})
	t.Run("invalid path values", func(t *testing.T) {
		cases := map[string]string{
			"empty":      "/api/v1/entries?path=",
			"multi":      "/api/v1/entries?path=%2F&path=%2F",
			"on_alias":   "/api/v1/list?path=",
			"on_unknown": "/api/v1/nope?path=",
		}
		for name, target := range cases {
			t.Run(name, func(t *testing.T) {
				router := readRouter(t, newReadDispatcher(t, &fakeReadStorage{}, nil))
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, newTestRequest("GET", target))
				want := `{"error":"query parameter 'path' must contain one non-empty path"}`
				if recorder.Code != http.StatusBadRequest || recorder.Body.String() != want {
					t.Fatalf("status = %d body = %q, want 400 %q", recorder.Code, recorder.Body.String(), want)
				}
			})
		}
	})
	t.Run("missing path parameter defaults to root", func(t *testing.T) {
		storage := newStorage()
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/entries"))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
		}
		if !strings.HasPrefix(recorder.Body.String(), `{"path":"/","entries":[`) {
			t.Errorf("body = %q", recorder.Body.String())
		}
	})
	t.Run("upstream errors map through the table", func(t *testing.T) {
		cases := []struct {
			name       string
			storage    *fakeReadStorage
			wantStatus int
			wantBody   string
			wantRetry  string
		}{
			{
				name:       "metadata 500",
				storage:    &fakeReadStorage{metadataErr: model.NewWpsAPIError("listing failed", 500, model.WpsCategoryUpstream)},
				wantStatus: http.StatusBadGateway,
				wantBody:   `{"error":"upstream WPS request failed","code":"wps_unavailable","upstream_status":500}`,
			},
			{
				name:       "list 500",
				storage:    &fakeReadStorage{metadataEntry: folderEntry("root-1"), listErr: model.NewWpsAPIError("listing failed", 500, model.WpsCategoryUpstream)},
				wantStatus: http.StatusBadGateway,
				wantBody:   `{"error":"upstream WPS request failed","code":"wps_unavailable","upstream_status":500}`,
			},
			{
				name:       "metadata 401",
				storage:    &fakeReadStorage{metadataErr: model.NewWpsAPIError("session expired", 401, model.WpsCategorySessionExpired)},
				wantStatus: http.StatusServiceUnavailable,
				wantBody:   `{"error":"WPS session expired; refresh the configured credentials","code":"wps_session_expired","upstream_status":401}`,
				wantRetry:  "60",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				router := readRouter(t, newReadDispatcher(t, tc.storage, nil))
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/entries?path=%2F"))
				if recorder.Code != tc.wantStatus || recorder.Body.String() != tc.wantBody {
					t.Fatalf("status = %d body = %q, want %d %q", recorder.Code, recorder.Body.String(), tc.wantStatus, tc.wantBody)
				}
				if got := recorder.Header().Get("Retry-After"); got != tc.wantRetry {
					t.Errorf("Retry-After = %q, want %q", got, tc.wantRetry)
				}
			})
		}
	})
	t.Run("path echo is the raw decoded value", func(t *testing.T) {
		storage := &fakeReadStorage{metadataEntry: folderEntry("root-1"), entries: []model.RemoteEntry{}}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/entries?path=%2Fbench-folder%2F"))
		want := `{"path":"/bench-folder/","entries":[]}`
		if recorder.Code != http.StatusOK || recorder.Body.String() != want {
			t.Fatalf("status = %d body = %q, want 200 %q", recorder.Code, recorder.Body.String(), want)
		}
		if len(storage.calls) != 2 || storage.calls[0] != "metadata:/bench-folder/" {
			t.Errorf("storage calls = %v", storage.calls)
		}
	})
}

// TestRESTMetadataRoute covers metadata: file and folder targets, the
// not-found and invalid-path cases, and upstream error mapping.
func TestRESTMetadataRoute(t *testing.T) {
	t.Run("file metadata keeps key order and nulls", func(t *testing.T) {
		storage := &fakeReadStorage{metadataEntry: fileEntry("id-1", "bench-one.txt")}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/metadata?path=%2Fbench-one.txt"))
		want := `{"path":"/bench-one.txt","entry":{"id":"id-1","name":"bench-one.txt","kind":"file","parent_id":"root-1","size":11,"modified_at":"2026-01-01T00:00:00Z","etag":"etag-1"}}`
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
		}
		if body := recorder.Body.String(); body != want {
			t.Errorf("body = %q, want %q", body, want)
		}
	})
	t.Run("folder metadata needs no kind check", func(t *testing.T) {
		storage := &fakeReadStorage{metadataEntry: folderEntry("root-1")}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/metadata?path=%2Fbench-folder"))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), `"kind":"folder"`) {
			t.Errorf("body = %q", recorder.Body.String())
		}
		if len(storage.calls) != 1 || storage.calls[0] != "metadata:/bench-folder" {
			t.Errorf("storage calls = %v", storage.calls)
		}
	})
	t.Run("missing and invalid paths", func(t *testing.T) {
		router := readRouter(t, newReadDispatcher(t, &fakeReadStorage{metadataErr: model.NewStorageError(model.KindEntryNotFound, "entry not found: missing.txt")}, nil))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/metadata?path=%2Fmissing.txt"))
		want := `{"error":"entry not found: missing.txt"}`
		if recorder.Code != http.StatusNotFound || recorder.Body.String() != want {
			t.Fatalf("status = %d body = %q, want 404 %q", recorder.Code, recorder.Body.String(), want)
		}
		// A relative path reaches the storage (Python's _query_path only
		// enforces non-emptiness); the storage rejects it with the
		// not-absolute rule.
		relative := readRouter(t, newReadDispatcher(t, &fakeReadStorage{metadataErr: model.NewStorageError(model.KindInvalidPath, "remote paths must start with '/'")}, nil))
		recorder = httptest.NewRecorder()
		relative.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/metadata?path=abc"))
		want400 := `{"error":"remote paths must start with '/'"}`
		if recorder.Code != http.StatusBadRequest || recorder.Body.String() != want400 {
			t.Fatalf("status = %d body = %q, want 400 %q", recorder.Code, recorder.Body.String(), want400)
		}
	})
	t.Run("upstream errors map through the table", func(t *testing.T) {
		storage := &fakeReadStorage{metadataErr: model.NewWpsAPIError("session expired", 401, model.WpsCategorySessionExpired)}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/metadata?path=%2Fx"))
		want := `{"error":"WPS session expired; refresh the configured credentials","code":"wps_session_expired","upstream_status":401}`
		if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != want {
			t.Fatalf("status = %d body = %q, want 503 %q", recorder.Code, recorder.Body.String(), want)
		}
		if got := recorder.Header().Get("Retry-After"); got != "60" {
			t.Errorf("Retry-After = %q", got)
		}
	})
}

// TestRESTDeferredRoutes pins the interim behavior of suffixes that land in
// later stages: unknown-route 404 after the shared path validation. The
// download route streams for real since B801.
func TestRESTDeferredRoutes(t *testing.T) {
	stream := newFakeStream("bench-bytes", model.Ptr(int64(11)))
	downloads := &downloadStorageFake{entry: downloadFileEntry(), stream: stream}
	router := readRouter(t, newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, downloads))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/download?path=%2Fbench-one.txt"))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "bench-bytes" {
		t.Fatalf("download status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	wantDisposition := `attachment; filename="download.txt"; filename*=UTF-8''bench-one.txt`
	if got := recorder.Header().Get("Content-Disposition"); got != wantDisposition {
		t.Fatalf("Content-Disposition = %q", got)
	}
	// The path query is validated before the route answers.
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/download?path="))
	want := `{"error":"query parameter 'path' must contain one non-empty path"}`
	if recorder.Code != http.StatusBadRequest || recorder.Body.String() != want {
		t.Fatalf("download invalid path = %d body = %q", recorder.Code, recorder.Body.String())
	}
	// A pathless unknown suffix defaults to "/" and still answers 404.
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/nope"))
	if recorder.Code != http.StatusNotFound || recorder.Body.String() != `{"error":"unknown REST route"}` {
		t.Fatalf("unknown status = %d body = %q", recorder.Code, recorder.Body.String())
	}
}

// TestRESTDispatcherRequiresStorage pins the constructor validation.
func TestRESTDispatcherRequiresStorage(t *testing.T) {
	limits := DefaultControlLimits()
	session, err := NewSessionImporter(limits, "",
		func([]any, string) (string, string, []string, error) {
			return "", "", nil, errBadRequest("unused in read tests")
		},
		&recordingCredentialReplacer{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewRootNameController(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRESTDispatcher(limits, controller, session, nil, nil, DownloadLimits{}, stubDownloadStorage{}, stubUploadStorage{}, stubMutations{}, newTestLockStore(t), 0); err == nil {
		t.Fatal("a nil storage must be rejected")
	}
}

// TestRESTReadUnderAuth walks one read route through the full middleware
// chain to confirm Basic Auth still covers the new routes.
func TestRESTReadUnderAuth(t *testing.T) {
	storage := &fakeReadStorage{metadataEntry: folderEntry("root-1"), entries: []model.RemoteEntry{fileEntry("id-1", "bench-one.txt")}}
	router := readRouter(t, newReadDispatcher(t, storage, nil))
	config := ChainConfig{
		Router: router,
		Health: func(w http.ResponseWriter, r *http.Request) {},
		Auth:   BasicAuthConfig{Username: "adapter", Password: "secret"},
	}
	chain, err := NewChain(config)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	chain.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/entries?path=%2F"))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated entries status = %d", recorder.Code)
	}
	request := newTestRequest("GET", "/api/v1/entries?path=%2F")
	request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("adapter:secret")))
	recorder = httptest.NewRecorder()
	chain.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated entries status = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"name":"bench-one.txt"`) {
		t.Errorf("body = %q", recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Length"); got != strconv.Itoa(recorder.Body.Len()) {
		t.Errorf("Content-Length = %q, want %d", got, recorder.Body.Len())
	}
}
