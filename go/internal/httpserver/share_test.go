package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/shares"
)

type publicShareBackend struct {
	mu        sync.Mutex
	entries   map[string]model.RemoteEntry
	children  map[string][]model.RemoteEntry
	data      map[string]string
	opened    []string
	stream    DownloadStream
	err       error
	afterOpen func()
	afterList func()
}

func (s *publicShareBackend) Metadata(path string) (model.RemoteEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return model.RemoteEntry{}, s.err
	}
	if entry, ok := s.entries[path]; ok {
		return entry, nil
	}
	return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "private upstream path="+path)
}
func (s *publicShareBackend) ListPath(path string) ([]model.RemoteEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.afterList != nil {
		s.afterList()
	}
	return append([]model.RemoteEntry(nil), s.children[path]...), s.err
}
func (s *publicShareBackend) ListChildren(path string, _ model.RemoteEntry) ([]model.RemoteEntry, error) {
	return s.ListPath(path)
}
func (s *publicShareBackend) OpenPath(ctx context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened = append(s.opened, path)
	if s.afterOpen != nil {
		s.afterOpen()
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.stream != nil {
		return s.stream, nil
	}
	body := s.data[path]
	if offset > int64(len(body)) {
		return nil, errors.New("range")
	}
	body = body[offset:]
	if length != nil {
		body = body[:*length]
	}
	return newFakeStream(body, model.Ptr(int64(len(body)))), nil
}

type shareFixture struct {
	controller *ShareController
	backend    *publicShareBackend
	owner      auth.Principal
	mu         sync.Mutex
	active     bool
	binding    string
}

func newShareFixture(t *testing.T) *shareFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	folder := archiveFolder("folder-id", "shared-folder")
	file := archiveFile("file-id", "100%25 + 文.txt", "shared-content")
	f := &shareFixture{active: true, binding: strings.Repeat("a", 64), owner: auth.Principal{ID: strings.Repeat("a", 32), Username: "alice", Role: "member", PolicyVersion: 1, RootPath: "/hidden/owner", RootID: "hidden-root-id", Permissions: auth.Permissions{Read: true}}, backend: &publicShareBackend{entries: map[string]model.RemoteEntry{"/folder": folder, "/folder/100%25 + 文.txt": file, "/outside": archiveFile("outside-id", "secret.txt", "private")}, children: map[string][]model.RemoteEntry{"/folder": {file}}, data: map[string]string{"/folder/100%25 + 文.txt": "shared-content", "/outside": "private"}}}
	controller, err := NewShareController(ShareConfig{File: filepath.Join(dir, "shares.json"), ResolveOwner: func(id string, version uint64) (auth.Principal, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.active || id != f.owner.ID || version != f.owner.PolicyVersion {
			return auth.Principal{}, shares.ErrDenied
		}
		return f.owner, nil
	}, StorageFor: func(auth.Principal) (ShareStorage, error) { return f.backend, nil }, Binding: func(auth.Principal, string) (string, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.binding, nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.controller = controller
	return f
}
func (f *shareFixture) manage(t *testing.T, method, suffix, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := writeRequest(method, "/api/v1/"+suffix, nil, body)
	r = r.WithContext(auth.WithPrincipal(r.Context(), f.owner))
	w := httptest.NewRecorder()
	if err := f.controller.ServeManagement(w, r, RESTRoute{Suffix: suffix}); err != nil {
		mapError(w, r, err, true)
	}
	return w
}
func (f *shareFixture) create(t *testing.T, path string) (shareMetadata, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"path": path})
	response := f.manage(t, "POST", "shares", string(body))
	if response.Code != 201 {
		t.Fatalf("create=%d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Share shareMetadata `json:"share"`
		URL   string        `json:"url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	_, token, ok := strings.Cut(payload.URL, "#")
	if !ok {
		t.Fatal("missing bearer fragment")
	}
	return payload.Share, token
}
func (f *shareFixture) public(t *testing.T, method, id, action, path, body, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/share/" + id + "/" + action
	if path != "" {
		target += "?" + url.Values{"path": {path}}.Encode()
	}
	r := writeRequest(method, target, nil, body)
	r.RemoteAddr = "192.0.2.3:1234"
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: shareCookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	if err := f.controller.ServePublic(w, r, id, action); err != nil {
		mapError(w, r, err, true)
	}
	return w
}
func (f *shareFixture) unlock(t *testing.T, id, token string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": token})
	response := f.public(t, "POST", id, "unlock", "", string(body), "")
	if response.Code != 200 {
		t.Fatalf("unlock=%d %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].Path != "/api/share/"+id || cookies[0].MaxAge > 3600 {
		t.Fatal(cookies)
	}
	return cookies[0].Value
}

func TestPublicFolderShareIsRelativeReadOnlyAndRedacted(t *testing.T) {
	f := newShareFixture(t)
	metadata, token := f.create(t, "/folder")
	cookie := f.unlock(t, metadata.ID, token)
	info := f.public(t, "GET", metadata.ID, "info", "/", "", cookie)
	if info.Code != 200 || strings.Contains(info.Body.String(), "hidden") || strings.Contains(info.Body.String(), "owner_id") || strings.Contains(info.Body.String(), "target_id") {
		t.Fatalf("info=%d %s", info.Code, info.Body.String())
	}
	listing := f.public(t, "GET", metadata.ID, "entries", "/", "", cookie)
	if listing.Code != 200 || strings.Contains(listing.Body.String(), "file-id") || strings.Contains(listing.Body.String(), "folder-id") || strings.Contains(listing.Body.String(), "parent_id") || strings.Contains(listing.Body.String(), "/folder/") {
		t.Fatalf("list=%d %s", listing.Code, listing.Body.String())
	}
	download := f.public(t, "GET", metadata.ID, "download", "/100%25 + 文.txt", "", cookie)
	if download.Code != 200 || download.Body.String() != "shared-content" {
		t.Fatalf("download=%d %q", download.Code, download.Body.String())
	}
	for _, action := range []string{"upload", "delete", "tasks", "search", "settings"} {
		response := f.public(t, "POST", metadata.ID, action, "/", "", cookie)
		if response.Code != 401 {
			t.Fatalf("public mutation %s=%d", action, response.Code)
		}
	}
	for _, path := range []string{"/../outside", "/a/../../outside", "relative", "/a\\b"} {
		response := f.public(t, "GET", metadata.ID, "download", path, "", cookie)
		if response.Code < 400 || strings.Contains(response.Body.String(), "private upstream") {
			t.Fatalf("invalid path %q=%d %s", path, response.Code, response.Body.String())
		}
	}
	if len(f.backend.opened) != 1 || f.backend.opened[0] != "/folder/100%25 + 文.txt" {
		t.Fatal(f.backend.opened)
	}
	list := f.manage(t, "GET", "shares", "")
	if strings.Contains(list.Body.String(), token) || strings.Contains(list.Body.String(), "token_hash") || strings.Contains(list.Body.String(), "binding") {
		t.Fatal("management response leaked bearer state", list.Body.String())
	}
}

func TestPublicSingleFileShareSupportsRangeAndRejectsChildren(t *testing.T) {
	f := newShareFixture(t)
	metadata, token := f.create(t, "/folder/100%25 + 文.txt")
	cookie := f.unlock(t, metadata.ID, token)
	r := writeRequest("GET", "/api/share/"+metadata.ID+"/download?path=/", map[string]string{"Range": "bytes=0-5"}, "")
	r.AddCookie(&http.Cookie{Name: shareCookieName, Value: cookie})
	w := httptest.NewRecorder()
	if err := f.controller.ServePublic(w, r, metadata.ID, "download"); err != nil {
		t.Fatal(err)
	}
	if w.Code != 206 || w.Body.String() != "shared" || w.Header().Get("Content-Range") != "bytes 0-5/14" {
		t.Fatalf("range=%d %q headers=%v", w.Code, w.Body.String(), w.Header())
	}
	if response := f.public(t, "GET", metadata.ID, "download", "/outside", "", cookie); response.Code != 401 {
		t.Fatalf("file escaped scope=%d", response.Code)
	}
	if response := f.public(t, "GET", metadata.ID, "entries", "/", "", cookie); response.Code != 409 {
		t.Fatalf("file listed as folder=%d", response.Code)
	}
}

func TestPublicShareRejectsRevokeOwnerPolicyTargetAndNamespaceChanges(t *testing.T) {
	for _, change := range []string{"revoke", "owner", "policy", "target", "binding"} {
		t.Run(change, func(t *testing.T) {
			f := newShareFixture(t)
			metadata, token := f.create(t, "/folder")
			cookie := f.unlock(t, metadata.ID, token)
			switch change {
			case "revoke":
				if response := f.manage(t, "DELETE", "shares/"+metadata.ID, ""); response.Code != 204 {
					t.Fatal(response.Body.String())
				}
			case "owner":
				f.active = false
			case "policy":
				f.owner.PolicyVersion++
			case "target":
				entry := f.backend.entries["/folder"]
				entry.ID = "replacement-id"
				f.backend.entries["/folder"] = entry
			case "binding":
				f.binding = strings.Repeat("b", 64)
			}
			response := f.public(t, "GET", metadata.ID, "info", "/", "", cookie)
			if response.Code != 401 || response.Body.String() != `{"error":"share is unavailable or access was denied"}` {
				t.Fatalf("denial=%d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestShareManagementCannotInspectOrRevokeAnotherOwner(t *testing.T) {
	f := newShareFixture(t)
	metadata, _ := f.create(t, "/folder")
	original := f.owner
	f.owner.ID = strings.Repeat("b", 32)
	f.owner.Username = "bob"
	list := f.manage(t, "GET", "shares", "")
	if list.Code != 200 || strings.Contains(list.Body.String(), metadata.ID) {
		t.Fatal(list.Body.String())
	}
	revoke := f.manage(t, "DELETE", "shares/"+metadata.ID, "")
	if revoke.Code != 404 {
		t.Fatal(revoke.Code, revoke.Body.String())
	}
	f.owner = original
	if _, err := f.controller.store.Active(metadata.ID); err != nil {
		t.Fatal("foreign user revoked share")
	}
}

func TestShareManagementHidesPreviousOwnerPolicyMetadata(t *testing.T) {
	f := newShareFixture(t)
	metadata, _ := f.create(t, "/folder")
	f.owner.PolicyVersion++
	response := f.manage(t, "GET", "shares", "")
	if response.Code != 200 || strings.Contains(response.Body.String(), metadata.ID) || strings.Contains(response.Body.String(), metadata.Name) || response.Body.String() != `{"shares":[]}` {
		t.Fatalf("previous root metadata leaked: %d %s", response.Code, response.Body.String())
	}
	// A known old ID can still be revoked without revealing its metadata.
	if revoke := f.manage(t, "DELETE", "shares/"+metadata.ID, ""); revoke.Code != 204 {
		t.Fatal(revoke.Code, revoke.Body.String())
	}
}

func TestShareStreamRevocationStopsSubsequentBytes(t *testing.T) {
	f := newShareFixture(t)
	metadata, token := f.create(t, "/folder/100%25 + 文.txt")
	cookie := f.unlock(t, metadata.ID, token)
	record, err := f.controller.store.Authorize(metadata.ID, cookie)
	if err != nil {
		t.Fatal(err)
	}
	view := &sharedReadView{controller: f.controller, record: record, grant: cookie}
	stream, err := view.OpenPath(context.Background(), "/", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	buffer := make([]byte, 3)
	if n, err := stream.Read(buffer); n != 3 || err != nil {
		t.Fatalf("first read=%d %v", n, err)
	}
	if err := f.controller.store.Revoke(metadata.ID, f.owner.ID, false); err != nil {
		t.Fatal(err)
	}
	if n, err := stream.Read(buffer); n != 0 || !errors.Is(err, shares.ErrDenied) {
		t.Fatalf("revoked read=%d %v", n, err)
	}
}

func TestPublicShareDownloadCancellationClosesSource(t *testing.T) {
	f := newShareFixture(t)
	metadata, token := f.create(t, "/folder/100%25 + 文.txt")
	cookie := f.unlock(t, metadata.ID, token)
	reader, writer := io.Pipe()
	defer writer.Close()
	stream := &cancelBlockingStream{fakeDownloadStream: newFakeStream("", nil), reader: reader, entered: make(chan struct{})}
	f.backend.stream = stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := writeRequest("GET", "/api/share/"+metadata.ID+"/download?path=/", nil, "").WithContext(ctx)
	r.AddCookie(&http.Cookie{Name: shareCookieName, Value: cookie})
	done := make(chan error, 1)
	go func() { done <- f.controller.ServePublic(httptest.NewRecorder(), r, metadata.ID, "download") }()
	select {
	case <-stream.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not close share source")
	}
	if stream.closeCalls() != 1 {
		t.Fatal(stream.closeCalls())
	}
}

func TestPublicShareErrorsDoNotExposeBackingDetails(t *testing.T) {
	f := newShareFixture(t)
	metadata, token := f.create(t, "/folder")
	cookie := f.unlock(t, metadata.ID, token)
	f.backend.err = errors.New("signed_url=https://private.example/?token=secret")
	response := f.public(t, "GET", metadata.ID, "entries", "/", "", cookie)
	if response.Code != 401 || strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "private.example") {
		t.Fatalf("error=%d %s", response.Code, response.Body.String())
	}
	response = f.public(t, "POST", metadata.ID, "unlock", "", `{"token":"`+strings.Repeat("0", 64)+`"}`, "")
	if response.Code != 401 || strings.Contains(response.Body.String(), "token=") {
		t.Fatal(response.Body.String())
	}
}

func TestShareStreamChecksRevocationAfterBlockedRead(t *testing.T) {
	f := newShareFixture(t)
	metadata, token := f.create(t, "/folder/100%25 + 文.txt")
	cookie := f.unlock(t, metadata.ID, token)
	record, err := f.controller.store.Authorize(metadata.ID, cookie)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	source := &cancelBlockingStream{fakeDownloadStream: newFakeStream("", nil), reader: reader, entered: make(chan struct{})}
	f.backend.stream = source
	view := &sharedReadView{controller: f.controller, record: record, grant: cookie}
	stream, err := view.OpenPath(context.Background(), "/", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() { n, err := stream.Read(make([]byte, 8)); done <- result{n, err} }()
	<-source.entered
	if err := f.controller.store.Revoke(metadata.ID, f.owner.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("secret")); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.n != 0 || !errors.Is(got.err, shares.ErrDenied) {
		t.Fatalf("post-revocation bytes escaped: %+v", got)
	}
}

func TestShareBearerNeverAppearsInRequestLogOrPublicMetadata(t *testing.T) {
	f := newShareFixture(t)
	metadata, token := f.create(t, "/folder")
	var logged []string
	handler := securityLog(func(_, method, path string) { logged = append(logged, method+" "+path) })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := f.controller.ServePublic(w, r, metadata.ID, "unlock"); err != nil {
			mapError(w, r, err, true)
		}
	}))
	r := writeRequest("POST", "/api/share/"+metadata.ID+"/unlock", nil, `{"token":"`+token+`"}`)
	r.RemoteAddr = "192.0.2.4:9999"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if strings.Contains(strings.Join(logged, "\n"), token) || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), "binding") || strings.Contains(w.Body.String(), "hidden") {
		t.Fatal("share capability or backing metadata leaked")
	}
}

func TestPublicShareClosesStreamIfTargetChangesDuringOpen(t *testing.T) {
	f := newShareFixture(t)
	metadata, token := f.create(t, "/folder/100%25 + 文.txt")
	cookie := f.unlock(t, metadata.ID, token)
	stream := newFakeStream("private replacement", model.Ptr(int64(19)))
	f.backend.stream = stream
	f.backend.afterOpen = func() {
		entry := f.backend.entries["/folder/100%25 + 文.txt"]
		entry.ID = "new-private-id"
		f.backend.entries["/folder/100%25 + 文.txt"] = entry
	}
	response := f.public(t, "GET", metadata.ID, "download", "/", "", cookie)
	if response.Code != 401 || strings.Contains(response.Body.String(), "private replacement") || stream.closeCalls() != 1 || len(stream.sizes()) != 0 {
		t.Fatalf("replacement escaped: status=%d body=%q closes=%d reads=%v", response.Code, response.Body.String(), stream.closeCalls(), stream.sizes())
	}
}

func TestPublicShareRejectsListingIfTargetChangesDuringList(t *testing.T) {
	f := newShareFixture(t)
	metadata, token := f.create(t, "/folder")
	cookie := f.unlock(t, metadata.ID, token)
	f.backend.afterList = func() {
		entry := f.backend.entries["/folder"]
		entry.ID = "new-private-folder"
		f.backend.entries["/folder"] = entry
		f.backend.children["/folder"] = []model.RemoteEntry{fileEntry("private-id", "private-name")}
	}
	response := f.public(t, "GET", metadata.ID, "entries", "/", "", cookie)
	if response.Code != 401 || strings.Contains(response.Body.String(), "private-name") {
		t.Fatalf("replacement listing escaped: %d %s", response.Code, response.Body.String())
	}
}

func TestFolderShareClosesStreamIfSelectedChildChangesDuringOpen(t *testing.T) {
	f := newShareFixture(t)
	metadata, token := f.create(t, "/folder")
	cookie := f.unlock(t, metadata.ID, token)
	stream := newFakeStream("changed child", model.Ptr(int64(13)))
	f.backend.stream = stream
	f.backend.afterOpen = func() {
		entry := f.backend.entries["/folder/100%25 + 文.txt"]
		entry.ID = "replacement-child"
		f.backend.entries["/folder/100%25 + 文.txt"] = entry
	}
	response := f.public(t, "GET", metadata.ID, "download", "/100%25 + 文.txt", "", cookie)
	if response.Code != 401 || stream.closeCalls() != 1 || len(stream.sizes()) != 0 {
		t.Fatalf("changed child escaped: %d %q close=%d", response.Code, response.Body.String(), stream.closeCalls())
	}
}
