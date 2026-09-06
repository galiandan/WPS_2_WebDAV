package httpserver

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

// cookieSelector adapts credentials.CredentialsFromCookies to the injected
// CookieSelector surface; the app assembly provides the same wrapper.
func cookieSelector(cookies []any, baseURL string) (string, string, []string, error) {
	snapshot, err := credentials.CredentialsFromCookies(cookies, baseURL)
	if err != nil {
		return "", "", nil, err
	}
	return snapshot.Credentials.Cookie, snapshot.Credentials.CSRFToken, snapshot.Names, nil
}

// importCookie builds one browser-shaped cookie object.
func importCookie(name, value, domain, path string) map[string]any {
	return map[string]any{"name": name, "value": value, "domain": domain, "path": path}
}

func importCookies() []any {
	return []any{
		importCookie("rtk", "refresh", ".kdocs.cn", "/passport/secure"),
		importCookie("csrf", "token", "365.kdocs.cn", "/"),
	}
}

// recordingCredentialReplacer records pair replacements.
type recordingCredentialSource struct {
	mu       sync.Mutex
	replaced []credentials.Credentials
	fail     bool
}

func (s *recordingCredentialSource) ReplaceCredentials(cookieHeader, csrfToken string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return false, nil
	}
	s.replaced = append(s.replaced, credentials.Credentials{Cookie: cookieHeader, CSRFToken: csrfToken})
	return true, nil
}

func (s *recordingCredentialSource) pairs() []credentials.Credentials {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]credentials.Credentials(nil), s.replaced...)
}

// recordingWorkspaceImporter records workspace updates over a real state.
type recordingWorkspaceImporter struct {
	state    *workspace.WorkspaceState
	updates  []string
	failNext bool
}

func (w *recordingWorkspaceImporter) Update(groupID, rootID string, spaces []workspace.Mount) (string, error) {
	if w.failNext {
		w.failNext = false
		return "", errors.New("workspace file is not writable")
	}
	if err := w.state.Update(groupID, rootID, spaces); err != nil {
		return "", err
	}
	root, err := w.state.RootID()
	if err != nil {
		return "", err
	}
	w.updates = append(w.updates, root)
	return root, nil
}

// recordingRootIDSetter records storage root switches.
type recordingRootIDSetter struct {
	roots []string
}

func (s *recordingRootIDSetter) SetRootID(rootID string) error {
	s.roots = append(s.roots, rootID)
	return nil
}

type importHarness struct {
	router    *Router
	source    *recordingCredentialSource
	workspace *recordingWorkspaceImporter
	roots     *recordingRootIDSetter
	state     *workspace.WorkspaceState
}

func newImportHarness(t *testing.T, withWorkspace bool) *importHarness {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	harness := &importHarness{source: &recordingCredentialSource{}, roots: &recordingRootIDSetter{}}
	configuredGroup, configuredRoot := "auto", "auto"
	state, err := workspace.NewWorkspaceState(filepath.Join(dir, "wps-workspace.json"), configuredGroup, configuredRoot)
	if err != nil {
		t.Fatal(err)
	}
	harness.state = state
	if withWorkspace {
		harness.workspace = &recordingWorkspaceImporter{state: state}
	}
	// The D-06 rule: the workspace surface exists only when the adapter
	// tracks a workspace state.
	var workspaceImport WorkspaceImporter
	var roots RootIDSetter
	if withWorkspace {
		workspaceImport = harness.workspace
		roots = harness.roots
	}
	limits := ControlLimits{MaxControlBody: 1024, MaxResponseBody: 4096}
	session, err := NewSessionImporter(limits, "https://365.kdocs.cn", cookieSelector, harness.source, workspaceImport, roots)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := workspace.NewWebSettings("", workspace.DefaultRootName)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewRootNameController(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewRESTDispatcher(limits, controller, session, &fakeReadStorage{}, nil, DownloadLimits{}, stubDownloadStorage{}, stubUploadStorage{}, stubMutations{}, newTestLockStore(t), 0)
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
			DAV: func(w http.ResponseWriter, r *http.Request, davPath string) error {
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.router = router
	return harness
}

// tooManySpaces is a JSON array of 129 space objects, one past the ceiling.
var tooManySpaces = func() string {
	items := make([]string, 0, workspace.MaxSpaces+1)
	for i := 0; i <= workspace.MaxSpaces; i++ {
		items = append(items, `{"group_id":"s`+strconv.Itoa(i)+`"}`)
	}
	return strings.Join(items, ",")
}()

func postImport(t *testing.T, harness *importHarness, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("POST", "/api/v1/session/import", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	recorder := httptest.NewRecorder()
	harness.router.ServeHTTP(recorder, request)
	return recorder
}

// TestSessionImportReplacesCredentialPair mirrors
// test_session_import_uses_basic_auth_and_replaces_credentials: the selected
// cookies become the stored pair, rtk keeps its /passport/secure path.
func TestSessionImportReplacesCredentialPair(t *testing.T) {
	harness := newImportHarness(t, false)
	payload := `{"cookies":[` +
		`{"name":"rtk","value":"refresh","domain":".kdocs.cn","path":"/passport/secure"},` +
		`{"name":"csrf","value":"token","domain":"365.kdocs.cn","path":"/"}]}`
	recorder := postImport(t, harness, payload)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != `{"status":"ok","cookie_count":2}` {
		t.Errorf("body = %q", body)
	}
	pairs := harness.source.pairs()
	if len(pairs) != 1 {
		t.Fatalf("replacements = %d", len(pairs))
	}
	if !strings.Contains(pairs[0].Cookie, "rtk=refresh") {
		t.Errorf("cookie header = %q", pairs[0].Cookie)
	}
	if pairs[0].CSRFToken != "token" {
		t.Errorf("csrf = %q", pairs[0].CSRFToken)
	}
}

// TestSessionImportPersistsWorkspaceAndSwitchesRoot mirrors
// test_session_import_persists_workspace_and_switches_root.
func TestSessionImportPersistsWorkspaceAndSwitchesRoot(t *testing.T) {
	harness := newImportHarness(t, true)
	payload := `{"cookies":[` +
		`{"name":"rtk","value":"refresh","domain":".kdocs.cn","path":"/"},` +
		`{"name":"csrf","value":"token","domain":"365.kdocs.cn","path":"/"}],` +
		`"workspace":{"group_id":"group-2","root_id":"root-3"}}`
	recorder := postImport(t, harness, payload)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != `{"status":"ok","cookie_count":2,"workspace":"updated"}` {
		t.Errorf("body = %q", body)
	}
	group, err := harness.state.GroupID()
	if err != nil || group != "group-2" {
		t.Errorf("state group = %q (err %v)", group, err)
	}
	root, err := harness.state.RootID()
	if err != nil || root != "root-3" {
		t.Errorf("state root = %q (err %v)", root, err)
	}
	if len(harness.roots.roots) != 1 || harness.roots.roots[0] != "root-3" {
		t.Errorf("storage root switches = %q", harness.roots.roots)
	}
}

// TestSessionImportSpaces pins the spaces array handling: defaults,
// duplicate names, and the 128-space ceiling.
func TestSessionImportSpaces(t *testing.T) {
	harness := newImportHarness(t, true)
	payload := `{"cookies":import_placeholder,"workspace":{"group_id":"g1","root_id":"r1",` +
		`"spaces":[{"group_id":"space-a"},{"group_id":"space-b","root_id":"r2","name":"B"}]}}`
	payload = strings.Replace(payload, "import_placeholder", `[
		{"name":"rtk","value":"refresh","domain":".kdocs.cn","path":"/"},
		{"name":"csrf","value":"token","domain":"365.kdocs.cn","path":"/"}]`, 1)
	recorder := postImport(t, harness, payload)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	spaces, err := harness.state.Spaces()
	if err != nil || len(spaces) != 2 {
		t.Fatalf("spaces = %q (err %v)", spaces, err)
	}
	if spaces[0].Name != "space-a" || spaces[0].RootID != "0" {
		t.Errorf("first space = %+v", spaces[0])
	}
	if spaces[1].Name != "B" || spaces[1].RootID != "r2" {
		t.Errorf("second space = %+v", spaces[1])
	}

	// Duplicate names are rejected before any write.
	harness = newImportHarness(t, true)
	duplicates := `{"cookies":[` +
		`{"name":"rtk","value":"refresh","domain":".kdocs.cn","path":"/"},` +
		`{"name":"csrf","value":"token","domain":"365.kdocs.cn","path":"/"}],` +
		`"workspace":{"group_id":"g1","root_id":"r1","spaces":[` +
		`{"group_id":"space-a"},{"name":"same","group_id":"space-b"},{"name":"same","group_id":"space-c"}]}}`
	recorder = postImport(t, harness, duplicates)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("duplicate names status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"error":"workspace spaces contain duplicate names"}` {
		t.Errorf("duplicate names body = %q", body)
	}
	if pairs := harness.source.pairs(); len(pairs) != 0 {
		t.Errorf("credentials were written despite the validation failure")
	}
}

// TestSessionImportValidation pins the input rules and the D-06 refusal.
func TestSessionImportValidation(t *testing.T) {
	validCookies := `[{"name":"rtk","value":"refresh","domain":".kdocs.cn","path":"/"},` +
		`{"name":"csrf","value":"token","domain":"365.kdocs.cn","path":"/"}]`
	cases := []struct {
		name     string
		payload  string
		withWS   bool
		wantCode int
		wantBody string
	}{
		{"missing cookies", `{}`, false, 400, `{"error":"JSON field 'cookies' must be a non-empty array"}`},
		{"empty cookies", `{"cookies":[]}`, false, 400, `{"error":"JSON field 'cookies' must be a non-empty array"}`},
		{"cookies not array", `{"cookies":{}}`, false, 400, `{"error":"JSON field 'cookies' must be a non-empty array"}`},
		{"workspace not object", `{"cookies":` + validCookies + `,"workspace":5}`, false, 400, `{"error":"JSON field 'workspace' must be an object"}`},
		{"workspace without state", `{"cookies":` + validCookies + `,"workspace":{"group_id":"g","root_id":"r"}}`, false, 400, `{"error":"workspace import requires WPS_GROUP_ID=auto or WPS_ROOT_ID=auto"}`},
		{"workspace null is absent", `{"cookies":` + validCookies + `,"workspace":null}`, false, 200, `{"status":"ok","cookie_count":2}`},
		{"bad group id", `{"cookies":` + validCookies + `,"workspace":{"group_id":"bad id","root_id":"r"}}`, true, 400, `{"error":"workspace.group_id is invalid"}`},
		{"missing group id", `{"cookies":` + validCookies + `,"workspace":{"root_id":"r"}}`, true, 400, `{"error":"workspace.group_id is invalid"}`},
		{"spaces empty", `{"cookies":` + validCookies + `,"workspace":{"group_id":"g","root_id":"r","spaces":[]}}`, true, 400, `{"error":"JSON field 'workspace.spaces' is invalid"}`},
		{"spaces too many", `{"cookies":` + validCookies + `,"workspace":{"group_id":"g","root_id":"r","spaces":[` + tooManySpaces + `]}}`, true, 400, `{"error":"JSON field 'workspace.spaces' is invalid"}`},
		{"space not object", `{"cookies":` + validCookies + `,"workspace":{"group_id":"g","root_id":"r","spaces":[3]}}`, true, 400, `{"error":"workspace space must be an object"}`},
		{"space without group", `{"cookies":` + validCookies + `,"workspace":{"group_id":"g","root_id":"r","spaces":[{"name":"x"}]}}`, true, 400, `{"error":"space.group_id is invalid"}`},
		{"space bad name", `{"cookies":` + validCookies + `,"workspace":{"group_id":"g","root_id":"r","spaces":[{"group_id":"s","name":"a/b"}]}}`, true, 400, `{"error":"space.name is invalid"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newImportHarness(t, tc.withWS)
			recorder := postImport(t, harness, tc.payload)
			if recorder.Code != tc.wantCode {
				t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
			}
			if body := recorder.Body.String(); body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// TestSessionImportTooManyCookies pins the 256-cookie ceiling.
func TestSessionImportTooManyCookies(t *testing.T) {
	harness := newImportHarness(t, false)
	cookies := make([]string, 0, 257)
	for i := 0; i < 257; i++ {
		cookies = append(cookies, `{"name":"c`+strconv.Itoa(i)+`","value":"v","domain":".kdocs.cn","path":"/"}`)
	}
	payload := `{"cookies":[` + strings.Join(cookies, ",") + `]}`
	recorder := postImport(t, harness, payload)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"error":"too many cookies"}` {
		t.Errorf("body = %q", body)
	}
}

// TestSessionImportOversizedBody pins the 512 KiB import body cap.
func TestSessionImportOversizedBody(t *testing.T) {
	harness := newImportHarness(t, false)
	filler := strings.Repeat("x", 600*1024)
	payload := `{"cookies":[],"filler":"` + filler + `"}`
	recorder := postImport(t, harness, payload)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"error":""}` {
		t.Errorf("body = %q", body)
	}
}

// TestSessionImportFixedErrors pins the redaction contract: cookie
// selection failures and store failures never echo detail to the client.
func TestSessionImportFixedErrors(t *testing.T) {
	harness := newImportHarness(t, false)
	// No WPS cookies among the payload: LoginError becomes a fixed 500.
	recorder := postImport(t, harness, `{"cookies":[{"name":"other","value":"v","domain":".kdocs.cn","path":"/"}]}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"error":"internal server error"}` {
		t.Errorf("body = %q", body)
	}

	// csrf missing: also a fixed 500.
	recorder = postImport(t, harness, `{"cookies":[{"name":"rtk","value":"v","domain":".kdocs.cn","path":"/"}]}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("csrf-missing status = %d", recorder.Code)
	}

	// A refusing credential store: fixed upstream 502.
	harness = newImportHarness(t, false)
	harness.source.fail = true
	recorder = postImport(t, harness, `{"cookies":`+`[{"name":"rtk","value":"v","domain":".kdocs.cn","path":"/"},{"name":"csrf","value":"c","domain":"365.kdocs.cn","path":"/"}]}`)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("store-failure status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"error":"upstream WPS request failed","code":"wps_unavailable"}` {
		t.Errorf("store-failure body = %q", body)
	}

	// A failing workspace write: credentials already replaced, then the
	// fixed upstream 502 (Python order, no rollback).
	harness = newImportHarness(t, true)
	harness.workspace.failNext = true
	payload := `{"cookies":[{"name":"rtk","value":"v","domain":".kdocs.cn","path":"/"},{"name":"csrf","value":"c","domain":"365.kdocs.cn","path":"/"}],` +
		`"workspace":{"group_id":"g","root_id":"r"}}`
	recorder = postImport(t, harness, payload)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("workspace-failure status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"error":"upstream WPS request failed","code":"wps_unavailable"}` {
		t.Errorf("workspace-failure body = %q", body)
	}
	if pairs := harness.source.pairs(); len(pairs) != 1 {
		t.Errorf("credentials must be replaced before the workspace write: %d", len(pairs))
	}
}

// TestCredentialsFromCookiesSelection pins the cookie selection rules
// directly: host/domain filtering, safety checks, rank preference, and the
// sort order.
func TestCredentialsFromCookiesSelection(t *testing.T) {
	snapshot, err := credentials.CredentialsFromCookies([]any{
		importCookie("csrf", "wrong-path", ".kdocs.cn", "/other"),
		importCookie("csrf", "exact", "365.kdocs.cn", "/"),
		importCookie("CSRF", "casefold", ".kdocs.cn", "/"),
		importCookie("rtk", "refresh", ".kdocs.cn", "/passport/secure/deep"),
		importCookie("foreign", "v", ".example.com", "/"),
		importCookie("bad;name", "v", ".kdocs.cn", "/"),
		importCookie("bad-value", "v;v", ".kdocs.cn", "/"),
		importCookie("ctrl", "v\x01", ".kdocs.cn", "/"),
		"not-an-object",
		importCookie("noname", "v", ".kdocs.cn", "/"),
	}, "https://365.kdocs.cn")
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Names; len(got) != 3 || got[0] != "csrf" || got[1] != "noname" || got[2] != "rtk" {
		t.Errorf("selected names = %q", got)
	}
	if !strings.Contains(snapshot.Credentials.Cookie, "csrf=exact") {
		t.Errorf("rank must prefer the exact-host cookie: %q", snapshot.Credentials.Cookie)
	}
	if strings.Contains(snapshot.Credentials.Cookie, "CSRF=") || strings.Contains(snapshot.Credentials.Cookie, "casefold") {
		t.Errorf("the case-folded loser must not survive selection: %q", snapshot.Credentials.Cookie)
	}
	if snapshot.Credentials.CSRFToken != "exact" {
		t.Errorf("csrf = %q", snapshot.Credentials.CSRFToken)
	}
	if !strings.Contains(snapshot.Credentials.Cookie, "rtk=refresh") {
		t.Errorf("rtk must be retained despite its /passport path: %q", snapshot.Credentials.Cookie)
	}
	if strings.Contains(snapshot.Credentials.Cookie, "foreign=") {
		t.Errorf("foreign-domain cookie leaked: %q", snapshot.Credentials.Cookie)
	}

	// rtk is required by default.
	_, err = credentials.CredentialsFromCookies([]any{
		importCookie("csrf", "token", ".kdocs.cn", "/"),
	}, "https://365.kdocs.cn")
	if err == nil {
		t.Fatal("missing rtk must fail")
	}
	var loginErr *credentials.LoginError
	if !errors.As(err, &loginErr) {
		t.Fatalf("error type = %T", err)
	}

	// The drive host itself is validated.
	_, err = credentials.CredentialsFromCookies(importCookies(), "http://365.kdocs.cn")
	if err == nil {
		t.Fatal("non-https base URL must fail")
	}
	_, err = credentials.CredentialsFromCookies(importCookies(), "https://evil.example/")
	if err == nil {
		t.Fatal("non-kdocs base URL must fail")
	}
}

// TestSessionImportUnderAuth proves the route sits behind Basic Auth in the
// full chain.
func TestSessionImportUnderAuth(t *testing.T) {
	harness := newImportHarness(t, false)
	config := ChainConfig{
		Router: harness.router,
		Health: func(w http.ResponseWriter, r *http.Request) {},
		Auth:   BasicAuthConfig{Username: "adapter", Password: "secret"},
	}
	chain, err := NewChain(config)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"cookies":[{"name":"rtk","value":"v","domain":".kdocs.cn","path":"/"},{"name":"csrf","value":"c","domain":"365.kdocs.cn","path":"/"}]}`
	request := httptest.NewRequest("POST", "/api/v1/session/import", strings.NewReader(body))
	request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	recorder := httptest.NewRecorder()
	chain.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated import status = %d", recorder.Code)
	}
	request = httptest.NewRequest("POST", "/api/v1/session/import", strings.NewReader(body))
	request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("adapter:secret")))
	recorder = httptest.NewRecorder()
	chain.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated import status = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	if len(harness.source.pairs()) == 0 {
		t.Errorf("credentials were not replaced")
	}
}

// TestSessionImportUnknownPOSTRoutes pins the unknown-POST behavior with
// body discard.
func TestSessionImportUnknownPOSTRoutes(t *testing.T) {
	harness := newImportHarness(t, false)
	body := `{"x":1}`
	request := httptest.NewRequest("POST", "/api/v1/unknown", strings.NewReader(body))
	request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	recorder := httptest.NewRecorder()
	harness.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"error":"unknown REST route"}` {
		t.Errorf("body = %q", body)
	}
	// Oversized bodies on unknown POST routes are refused by the discard.
	big := `{"filler":"` + strings.Repeat("x", 2048) + `"}`
	request = httptest.NewRequest("POST", "/api/v1/unknown", strings.NewReader(big))
	request.Header.Set("Content-Length", strconv.Itoa(len(big)))
	recorder = httptest.NewRecorder()
	harness.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized discard status = %d", recorder.Code)
	}
}
