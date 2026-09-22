package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/accounts"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

// Requests travel through the real router, Basic/session middleware, account
// store, scoped services, MultiSpace, WPS request parser and object transport.
// Only the remote WPS transports are replaced; no live account is contacted.
type scopedWPS struct {
	mu                           sync.Mutex
	children                     map[string][]map[string]any
	contents                     map[string]string
	deletes                      []string
	deleteStarted, deleteRelease chan struct{}
	deleteOnce                   sync.Once
}

func scopedWPSFixture() *scopedWPS {
	folder := func(id, name string) map[string]any {
		return map[string]any{"id": id, "fname": name, "ftype": "folder"}
	}
	file := func(id, name, parent string) map[string]any {
		return map[string]any{"id": id, "fname": name, "ftype": "file", "parentid": parent, "fsize": int64(len("content-" + id)), "fsha": "hash-" + id}
	}
	return &scopedWPS{children: map[string][]map[string]any{
		"0":   {folder("101", "alpha"), folder("102", "beta")},
		"101": {file("201", "shared.txt", "101"), file("211", "alpha-only.txt", "101")},
		"102": {file("202", "shared.txt", "102"), file("212", "beta-only.txt", "102")},
	}, contents: map[string]string{"201": "content-201", "202": "content-202", "211": "content-211", "212": "content-212"}}
}

func scopedJSONResponse(payload any) *http.Response {
	body, _ := json.Marshal(payload)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

func (s *scopedWPS) Do(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/download") {
		parts := strings.Split(r.URL.Path, "/")
		id := parts[len(parts)-2]
		s.mu.Lock()
		_, ok := s.contents[id]
		s.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("unexpected file id %q", id)
		}
		return scopedJSONResponse(map[string]any{"download_url": "https://objects.kdocs.cn/" + id}), nil
	}
	if strings.HasSuffix(r.URL.Path, "/files") && r.Method == "GET" {
		s.mu.Lock()
		defer s.mu.Unlock()
		children := s.children[r.URL.Query().Get("parentid")]
		if children == nil {
			children = []map[string]any{}
		}
		return scopedJSONResponse(map[string]any{"files": children, "result": "ok", "next_offset": -1}), nil
	}
	if strings.HasSuffix(r.URL.Path, "/task/delete") {
		var payload struct {
			FileIDs []json.RawMessage `json:"fileids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return nil, err
		}
		s.mu.Lock()
		for _, raw := range payload.FileIDs {
			s.deletes = append(s.deletes, strings.Trim(string(raw), `"`))
		}
		started, release := s.deleteStarted, s.deleteRelease
		s.mu.Unlock()
		if release != nil {
			s.deleteOnce.Do(func() { close(started); <-release })
		}
		return scopedJSONResponse(map[string]any{"result": "ok", "taskuuid": "offline-task"}), nil
	}
	if strings.HasSuffix(r.URL.Path, "/task/progress") {
		return scopedJSONResponse(map[string]any{"result": "ok", "finish": 1}), nil
	}
	return nil, fmt.Errorf("unexpected fake WPS request: %s %s", r.Method, r.URL.Path)
}

func (s *scopedWPS) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "objects.kdocs.cn" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
		return nil, fmt.Errorf("unexpected object request")
	}
	s.mu.Lock()
	body, ok := s.contents[strings.TrimPrefix(r.URL.Path, "/")]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("missing object")
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/plain"}, "Content-Length": {strconv.Itoa(len(body))}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
}

type scopedAppHarness struct {
	app        *Application
	handler    http.Handler
	remote     *scopedWPS
	alice, bob accounts.User
	taskFile   string
}

func newScopedAppHarness(t *testing.T) *scopedAppHarness {
	t.Helper()
	cfg := withAuth(t, withCredentials(t, withWorkspace(t, fixtureConfig(t), `{"group_id":"group-1","root_id":"0"}`)))
	cfg.TasksFile = filepath.Join(filepath.Dir(cfg.WebSettingsDir), "member-tasks.json")
	remote := scopedWPSFixture()
	application, err := New(cfg, "test", WithTransports(remote, remote))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(application.Close)
	aliceBinding, err := application.rootScopeBinding("/alpha")
	if err != nil {
		t.Fatal(err)
	}
	bobBinding, err := application.rootScopeBinding("/beta")
	if err != nil {
		t.Fatal(err)
	}
	alice, err := application.Accounts.Create(accounts.CreateUser{Username: "alice", Password: "alice-password", RootPath: "/alpha", RootID: "101", RootBinding: aliceBinding, Permissions: auth.Permissions{Read: true, Upload: true, Delete: true}})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := application.Accounts.Create(accounts.CreateUser{Username: "bob", Password: "bob-password", RootPath: "/beta", RootID: "102", RootBinding: bobBinding, Permissions: auth.Permissions{Read: true, Upload: true, Delete: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := application.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return &scopedAppHarness{application, handler, remote, alice, bob, cfg.TasksFile}
}

func (h *scopedAppHarness) request(method, target, user string, cookie *http.Cookie, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://adapter.invalid"+target, strings.NewReader(body))
	r.Header.Set("Origin", "http://adapter.invalid")
	if body != "" {
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
		r.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		password := user + "-password"
		if user == "adapter" {
			password = "secret"
		}
		r.SetBasicAuth(user, password)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func (h *scopedAppHarness) login(t *testing.T, user string) *http.Cookie {
	t.Helper()
	w := h.request("POST", "/api/v1/auth/login", "", nil, `{"username":"`+user+`","password":"`+user+`-password"}`, nil)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "/alpha") || strings.Contains(w.Body.String(), "/beta") || strings.Contains(w.Body.String(), `"root_id"`) || strings.Contains(w.Body.String(), `"root_binding"`) {
		t.Fatalf("session exposes backing root: %s", w.Body.String())
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == auth.SessionCookieName {
			return cookie
		}
	}
	t.Fatal("login did not create session")
	return nil
}

func TestScopedRoutesBasicSessionReadsAndArchiveIsolation(t *testing.T) {
	h := newScopedAppHarness(t)
	for _, member := range []struct{ name, id, other string }{{"alice", "201", "beta-only.txt"}, {"bob", "202", "alpha-only.txt"}} {
		cookie := h.login(t, member.name)
		for _, viaSession := range []bool{false, true} {
			user, session := member.name, (*http.Cookie)(nil)
			if viaSession {
				user, session = "", cookie
			}
			for _, route := range []string{"entries?path=/", "metadata?path=/", "metadata?path=/shared.txt"} {
				w := h.request("GET", "/api/v1/"+route, user, session, "", nil)
				if w.Code != 200 || strings.Contains(w.Body.String(), member.other) || strings.Contains(w.Body.String(), "/alpha") || strings.Contains(w.Body.String(), "/beta") {
					t.Fatalf("member %s session=%t %s: %d %s", member.name, viaSession, route, w.Code, w.Body.String())
				}
				if route == "metadata?path=/shared.txt" && !strings.Contains(w.Body.String(), `"parent_id":null`) {
					t.Fatal("parent id exposed")
				}
			}
			for _, route := range []string{"preview", "download", "text"} {
				w := h.request("GET", "/api/v1/"+route+"?path=/shared.txt", user, session, "", nil)
				if w.Code != 200 || w.Body.String() != "content-"+member.id {
					t.Fatalf("member %s %s body: %d %s", member.name, route, w.Code, w.Body.String())
				}
			}
			w := h.request("GET", "/api/v1/archive?path=/shared.txt", user, session, "", nil)
			archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
			if w.Code != 200 || err != nil || len(archive.File) != 1 {
				t.Fatalf("archive: %d %v", w.Code, err)
			}
			stream, err := archive.File[0].Open()
			if err != nil {
				t.Fatal(err)
			}
			contents, _ := io.ReadAll(stream)
			stream.Close()
			if archive.File[0].Name != "shared.txt" || string(contents) != "content-"+member.id {
				t.Fatal("archive exposed another root")
			}
		}
		w := h.request("PROPFIND", "/dav/", member.name, nil, "", map[string]string{"Depth": "infinity"})
		if w.Code != 207 || strings.Contains(w.Body.String(), member.other) || strings.Contains(w.Body.String(), "/alpha/") || strings.Contains(w.Body.String(), "/beta/") || !strings.Contains(w.Body.String(), "/dav/shared.txt") {
			t.Fatalf("DAV tree isolation: %d %s", w.Code, w.Body.String())
		}
		w = h.request("GET", "/dav/shared.txt", member.name, nil, "", nil)
		if w.Code != 200 || w.Body.String() != "content-"+member.id {
			t.Fatalf("DAV download: %d %s", w.Code, w.Body.String())
		}
	}
	for _, target := range []string{"/api/v1/entries?path=/../beta", "/api/v1/metadata?path=/beta/shared.txt", "/dav/../beta/shared.txt", "/dav/beta/shared.txt"} {
		w := h.request("GET", target, "alice", nil, "", nil)
		if w.Code == 200 || strings.Contains(w.Body.String(), "content-202") {
			t.Fatalf("escape: %s %d %s", target, w.Code, w.Body.String())
		}
	}
}

func TestScopedRoutesAdministrativeAndWritePermissions(t *testing.T) {
	h := newScopedAppHarness(t)
	for _, request := range []struct{ method, path string }{{"GET", "users"}, {"GET", "storage"}, {"GET", "update"}, {"PATCH", "settings"}, {"PATCH", "storage"}, {"POST", "session/import"}, {"POST", "update"}, {"POST", "users"}} {
		w := h.request(request.method, "/api/v1/"+request.path, "alice", nil, "{}", nil)
		if request.method == "GET" {
			w = h.request(request.method, "/api/v1/"+request.path, "alice", nil, "", nil)
		}
		if w.Code != 403 {
			t.Fatalf("member admin route: %s %s => %d %s", request.method, request.path, w.Code, w.Body.String())
		}
	}
	if w := h.request("GET", "/api/v1/users", "adapter", nil, "", nil); w.Code != 200 {
		t.Fatalf("admin users denied: %d %s", w.Code, w.Body.String())
	}
	readOnly := auth.Permissions{Read: true}
	if _, err := h.app.Accounts.Update(h.alice.ID, accounts.UpdateUser{Permissions: &readOnly}); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct{ method, path, body string }{{"DELETE", "/api/v1/entries?path=/shared.txt", ""}, {"PUT", "/api/v1/upload?path=/new.txt", "new"}, {"POST", "/api/v1/folders?path=/new", ""}, {"POST", "/api/v1/tasks", `{"operation":"delete","paths":["/shared.txt"]}`}, {"POST", "/api/v1/batch", `{"operation":"delete","paths":["/shared.txt"]}`}, {"DELETE", "/dav/shared.txt", ""}, {"PUT", "/dav/new.txt", "new"}, {"MKCOL", "/dav/new", ""}, {"LOCK", "/dav/shared.txt", ""}} {
		w := h.request(request.method, request.path, "alice", nil, request.body, nil)
		if w.Code != 403 {
			t.Fatalf("read-only mutation %s %s => %d %s", request.method, request.path, w.Code, w.Body.String())
		}
	}
	w := h.request("GET", "/api/v1/tasks", "alice", nil, "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"tasks":[]`) {
		t.Fatalf("forbidden task was queued: %d %s", w.Code, w.Body.String())
	}
	h.remote.mu.Lock()
	deletes := len(h.remote.deletes)
	h.remote.mu.Unlock()
	if deletes != 0 {
		t.Fatal("read-only mutation reached WPS")
	}
	read := h.request("GET", "/api/v1/text?path=/shared.txt", "alice", nil, "", nil)
	edit := h.request("PUT", "/api/v1/text?path=/shared.txt", "alice", nil, "new", map[string]string{"If-Match": read.Header().Get("ETag"), "Content-Type": "text/plain; charset=utf-8"})
	if read.Code != 200 || edit.Code != 403 {
		t.Fatalf("read-only editor write should be a known permission refusal: read=%d save=%d %s", read.Code, edit.Code, edit.Body.String())
	}
}

func TestScopedRoutesSearchAndLockIsolation(t *testing.T) {
	h := newScopedAppHarness(t)
	for _, user := range []string{"alice", "bob"} {
		w := h.request("POST", "/api/v1/search/refresh?path=/", user, nil, "", nil)
		if w.Code != 202 {
			t.Fatalf("start search %s: %d %s", user, w.Code, w.Body.String())
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			w = h.request("GET", "/api/v1/search?type=file", user, nil, "", nil)
			if w.Code != 200 {
				t.Fatalf("search: %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), `"complete":true`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("scan unfinished: %s", w.Body.String())
			}
			time.Sleep(time.Millisecond)
		}
		other := "beta-only.txt"
		if user == "bob" {
			other = "alpha-only.txt"
		}
		if strings.Contains(w.Body.String(), other) || strings.Contains(w.Body.String(), "/alpha/") || strings.Contains(w.Body.String(), "/beta/") {
			t.Fatalf("search leaked: %s", w.Body.String())
		}
	}
	lock := h.request("LOCK", "/dav/shared.txt", "alice", nil, "", nil)
	if lock.Code != 200 || strings.Contains(lock.Body.String(), "/alpha/") {
		t.Fatalf("member lock: %d %s", lock.Code, lock.Body.String())
	}
	w := h.request("DELETE", "/api/v1/entries?path=/alpha/shared.txt", "adapter", nil, "", nil)
	if w.Code != 423 {
		t.Fatalf("admin bypassed member lock: %d %s", w.Code, w.Body.String())
	}
	w = h.request("GET", "/dav/shared.txt", "bob", nil, "", nil)
	if w.Code != 200 || w.Body.String() != "content-202" {
		t.Fatal("another root lock polluted Bob namespace")
	}
}

func TestScopedRoutesRootReplacementAndSessionRevocation(t *testing.T) {
	h := newScopedAppHarness(t)
	cookie := h.login(t, "alice")
	if w := h.request("GET", "/api/v1/entries", "", cookie, "", nil); w.Code != 200 {
		t.Fatal("initial member session unusable")
	}
	h.remote.mu.Lock()
	h.remote.children["0"][0]["id"] = "999"
	h.remote.children["999"] = h.remote.children["102"]
	h.remote.mu.Unlock()
	for _, route := range []string{"/api/v1/entries?path=/", "/api/v1/preview?path=/shared.txt", "/dav/shared.txt"} {
		w := h.request("GET", route, "alice", nil, "", nil)
		if w.Code != 403 {
			t.Fatalf("replaced root exposed %s: %d %s", route, w.Code, w.Body.String())
		}
	}
	h.remote.mu.Lock()
	h.remote.children["0"][0]["id"] = "101"
	h.remote.mu.Unlock()
	permissions := auth.Permissions{Read: true}
	if _, err := h.app.Accounts.Update(h.alice.ID, accounts.UpdateUser{Permissions: &permissions}); err != nil {
		t.Fatal(err)
	}
	w := h.request("GET", "/api/v1/entries", "", cookie, "", nil)
	if w.Code != 401 {
		t.Fatalf("stale session remained authenticated: %d %s", w.Code, w.Body.String())
	}
	enabled := false
	if _, err := h.app.Accounts.Update(h.alice.ID, accounts.UpdateUser{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	w = h.request("GET", "/dav/shared.txt", "alice", nil, "", nil)
	if w.Code != 401 {
		t.Fatalf("disabled Basic account remained authenticated: %d %s", w.Code, w.Body.String())
	}
}

func TestScopedRoutesTaskOwnershipAndQueuedRevocation(t *testing.T) {
	h := newScopedAppHarness(t)
	h.remote.deleteStarted, h.remote.deleteRelease = make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-h.remote.deleteRelease:
		default:
			close(h.remote.deleteRelease)
		}
	}()
	submit := func(path string) string {
		t.Helper()
		w := h.request("POST", "/api/v1/tasks", "alice", nil, `{"operation":"delete","paths":["`+path+`"]}`, nil)
		var payload struct {
			Task struct {
				ID string `json:"id"`
			} `json:"task"`
		}
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &payload) != nil || payload.Task.ID == "" {
			t.Fatalf("task submit: %d %s", w.Code, w.Body.String())
		}
		return payload.Task.ID
	}
	first := submit("/shared.txt")
	select {
	case <-h.remote.deleteStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("task never reached owned fake source")
	}
	second := submit("/alpha-only.txt")
	for _, method := range []string{"GET", "POST"} {
		for _, suffix := range []string{"", "/cancel", "/retry"} {
			if method == "GET" && suffix != "" {
				continue
			}
			w := h.request(method, "/api/v1/tasks/"+first+suffix, "bob", nil, "", nil)
			if w.Code != 404 {
				t.Fatalf("Bob accessed Alice task %s%s: %d %s", method, suffix, w.Code, w.Body.String())
			}
		}
	}
	w := h.request("GET", "/api/v1/tasks", "bob", nil, "", nil)
	if strings.Contains(w.Body.String(), first) || !strings.Contains(w.Body.String(), `"tasks":[]`) {
		t.Fatal("other member task history leaked")
	}
	readOnly := auth.Permissions{Read: true}
	if _, err := h.app.Accounts.Update(h.alice.ID, accounts.UpdateUser{Permissions: &readOnly}); err != nil {
		t.Fatal(err)
	}
	close(h.remote.deleteRelease)
	deadline := time.Now().Add(3 * time.Second)
	for {
		raw, err := os.ReadFile(h.taskFile)
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			Records []struct {
				Task struct {
					ID, State string
					Items     []struct{ Status int }
				} `json:"task"`
			} `json:"records"`
		}
		if json.Unmarshal(raw, &state) != nil {
			t.Fatal("invalid task journal")
		}
		finished := false
		for _, record := range state.Records {
			if record.Task.ID == second && record.Task.State == "failed" {
				finished = true
				if len(record.Task.Items) != 1 || record.Task.Items[0].Status != 403 {
					t.Fatalf("revoked task not denied: %+v", record)
				}
			}
		}
		if finished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("revoked queued task did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	h.remote.mu.Lock()
	deletes := append([]string(nil), h.remote.deletes...)
	h.remote.mu.Unlock()
	if len(deletes) != 1 || deletes[0] != "201" {
		t.Fatalf("wrong account or revoked item reached remote: %v", deletes)
	}
	w = h.request("GET", "/api/v1/tasks/"+second, "alice", nil, "", nil)
	if w.Code != 404 {
		t.Fatalf("new policy exposed old task: %d %s", w.Code, w.Body.String())
	}
}

func TestScopedRoutesRootZeroNamespaceRemapAndCookieRefresh(t *testing.T) {
	h := newScopedAppHarness(t)
	if err := h.app.State.Update("old-group", "0", []workspace.Mount{{Name: "Same", GroupID: "old-group", RootID: "0"}}); err != nil {
		t.Fatal(err)
	}
	binding, err := h.app.rootScopeBinding("/Same")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.app.Accounts.Create(accounts.CreateUser{Username: "whole", Password: "whole-password", RootPath: "/Same", RootID: "0", RootBinding: binding, Permissions: auth.Permissions{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	w := h.request("GET", "/api/v1/preview?path=/alpha/shared.txt", "whole", nil, "", nil)
	if w.Code != 200 || w.Body.String() != "content-201" {
		t.Fatalf("original root0 grant failed: %d %s", w.Code, w.Body.String())
	}
	if err := os.WriteFile(h.app.Config.CookieFile, []byte("wps_cookie=rotated-sanitized-session; csrf=fake-csrf"), 0o600); err != nil {
		t.Fatal(err)
	}
	w = h.request("GET", "/api/v1/preview?path=/alpha/shared.txt", "whole", nil, "", nil)
	if w.Code != 200 {
		t.Fatalf("normal cookie refresh revoked logical namespace: %d %s", w.Code, w.Body.String())
	}
	if err := h.app.State.Update("replacement-group", "0", []workspace.Mount{{Name: "Same", GroupID: "replacement-group", RootID: "0"}}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/api/v1/entries?path=/", "/api/v1/preview?path=/alpha/shared.txt", "/dav/alpha/shared.txt"} {
		w = h.request("GET", target, "whole", nil, "", nil)
		if w.Code != 403 {
			t.Fatalf("same-name/root0 remap reused grant: %s %d %s", target, w.Code, w.Body.String())
		}
	}
}

func TestScopedRoutesAdminCreatesConcreteWholeRootOnly(t *testing.T) {
	h := newScopedAppHarness(t)
	create := func(name string) *httptest.ResponseRecorder {
		return h.request("POST", "/api/v1/users", "adapter", nil, `{"username":"`+name+`","password":"`+name+`-password","root_path":"/","permissions":{"read":true,"upload":false,"delete":false}}`, nil)
	}
	w := create("whole")
	if w.Code != 201 {
		t.Fatalf("whole single-space root create failed: %d %s", w.Code, w.Body.String())
	}
	principal, ok := h.app.Accounts.LookupUsername("whole")
	if !ok || principal.RootID != "0" || len(principal.RootBinding) != 64 {
		t.Fatalf("scope not bound to real root: %+v", principal)
	}
	cookie := h.login(t, "whole")
	w = h.request("GET", "/api/v1/download?path=/alpha/shared.txt", "", cookie, "", nil)
	if w.Code != 200 || w.Body.String() != "content-201" {
		t.Fatalf("whole scope unusable: %d %s", w.Code, w.Body.String())
	}
	if err := h.app.State.Update("g1", "0", []workspace.Mount{{Name: "One", GroupID: "g1", RootID: "0"}, {Name: "Two", GroupID: "g2", RootID: "0"}}); err != nil {
		t.Fatal(err)
	}
	w = create("virtual")
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("virtual all-space root must reject account creation: %d %s", w.Code, w.Body.String())
	}
	if _, ok := h.app.Accounts.LookupUsername("virtual"); ok {
		t.Fatal("synthetic-root user persisted")
	}
}

func TestScopedRoutesEditorRevisionCannotCrossAccounts(t *testing.T) {
	h := newScopedAppHarness(t)
	// Even identical visible paths, bytes and remote IDs must not share an
	// editor revision across principals with different directory grants.
	h.remote.mu.Lock()
	h.remote.children["102"][0]["id"] = "201"
	h.remote.children["102"][0]["fsha"] = "hash-201"
	h.remote.mu.Unlock()
	alice := h.request("GET", "/api/v1/text?path=/shared.txt", "alice", nil, "", nil)
	bob := h.request("GET", "/api/v1/text?path=/shared.txt", "bob", nil, "", nil)
	if alice.Code != 200 || bob.Code != 200 || alice.Body.String() != bob.Body.String() || alice.Header().Get("ETag") == bob.Header().Get("ETag") {
		t.Fatalf("member editor revisions not isolated: %d/%d", alice.Code, bob.Code)
	}
	w := h.request("PUT", "/api/v1/text?path=/shared.txt", "bob", nil, "changed", map[string]string{"If-Match": alice.Header().Get("ETag"), "Content-Type": "text/plain; charset=utf-8"})
	if w.Code != 412 {
		t.Fatalf("another member revision accepted: %d %s", w.Code, w.Body.String())
	}
}
