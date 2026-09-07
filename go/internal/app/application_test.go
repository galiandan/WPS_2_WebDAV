package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/config"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/web"
)

// fixtureConfig mirrors config.Load's defaults over a private temporary
// directory, so the assembly reads only local fixtures.
func fixtureConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	// The secure-file validators require a private parent directory;
	// t.TempDir itself is world-readable here, so the fixtures live in a
	// 0700 subdirectory.
	private := filepath.Join(dir, "secrets")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		GroupID:        "",
		RootID:         "0",
		WorkspaceFile:  filepath.Join(private, "workspace.json"),
		CookieFile:     "",
		CSRFTokenFile:  "",
		RefreshTimeout: 30,
		BaseURL:        "https://365.kdocs.cn",
		ObjectSuffix:   "kdocs.cn",
		AutoRefresh:    true,
		EnableRange:    true,
		Timeout:        30,
		StatusProbeTTL: 30,

		UploadSpoolMemory:  8 << 20,
		StreamChunkSize:    1 << 20,
		MultipartThreshold: 50 << 20,
		MultipartPartSize:  10 << 20,
		MaxUploadBytes:     512 << 20,
		UploadRetries:      2,
		UploadRetryDelay:   0.5,
		MaxJSONResponse:    8 << 20,

		RootName:         config.DefaultRootName,
		ListCount:        20,
		MaxListEntries:   10000,
		CacheTTL:         2.0,
		MaxCachedFolders: 1024,
		MaxUploads:       2,
		MaxDownloads:     4,
		TransferWait:     30.0,
		MaxCopyEntries:   10000,
		MaxCopyDepth:     64,

		MaxPropfindEntries: 10000,
		MaxPropfindDepth:   64,
		MaxControlBody:     1 << 20,
		MaxResponseBody:    16 << 20,
		MaxLocks:           4096,

		WebSettingsDir: filepath.Join(private, "web-settings.json"),
		DAVPrefix:      "/dav",
		RESTPrefix:     "/api/v1",
		Bind:           "127.0.0.1",
		Port:           54321,
		MaxConnections: 64,
		RequestTimeout: 60.0,
	}
	// The real environment always yields a workspace state (the default
	// group id counts as auto), so the fixture starts from a minimal one.
	if err := os.WriteFile(cfg.WorkspaceFile, []byte(`{"group_id": "group-1", "root_id": "root-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// withWorkspace writes a workspace fixture and points the config at it.
func withWorkspace(t *testing.T, cfg config.Config, payload string) config.Config {
	t.Helper()
	if err := os.WriteFile(cfg.WorkspaceFile, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// withCredentials writes the credential fixture pair; the values are
// invented and never leave the test.
func withCredentials(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	cfg.CookieFile = filepath.Join(filepath.Dir(cfg.WorkspaceFile), "wps-cookie.txt")
	if err := os.WriteFile(cfg.CookieFile, []byte("wps_cookie=fake-session; rtk=fake-rtk; csrf=fake-csrf"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.CSRFTokenFile = cfg.CookieFile + ".csrf"
	if err := os.WriteFile(cfg.CSRFTokenFile, []byte("fake-csrf"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func withAuth(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	cfg.Username = "adapter"
	cfg.Password = "secret"
	return cfg
}

func TestNewAssemblesServices(t *testing.T) {
	cfg := withCredentials(t, withWorkspace(t, fixtureConfig(t),
		`{"group_id": "group-1", "root_id": "root-1"}`))
	// With an auto root the workspace file's selection applies.
	cfg.RootID = "auto"
	application, err := New(cfg, "test-1")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer application.Close()

	if application.Settings == nil || application.State == nil || application.Source == nil ||
		application.Client == nil || application.Budget == nil || application.Storage == nil ||
		application.Locks == nil {
		t.Fatal("the assembly left a service unwired")
	}
	// The settings file is missing, so the fallback name wins and the
	// virtual root reports it; the workspace root id feeds the single view.
	name, err := application.Settings.Name()
	if err != nil {
		t.Fatal(err)
	}
	root, err := application.Storage.Root()
	if err != nil {
		t.Fatalf("storage root: %v", err)
	}
	if root.Name != name || root.Kind != "folder" {
		t.Fatalf("storage root = %+v", root)
	}
	statusRoot, err := application.Storage.StatusRootID()
	if err != nil {
		t.Fatal(err)
	}
	if statusRoot != "root-1" {
		t.Fatalf("status root id = %q", statusRoot)
	}
	if application.DAVPrefix() != "/dav" || application.RESTPrefix() != "/api/v1" {
		t.Fatalf("prefixes = %q / %q", application.DAVPrefix(), application.RESTPrefix())
	}
}

func TestNewAppliesWebSettingsName(t *testing.T) {
	cfg := withWorkspace(t, fixtureConfig(t), `{"group_id": "g", "root_id": "r"}`)
	if err := os.WriteFile(cfg.WebSettingsDir, []byte(`{"name": "Test Drive"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := New(cfg, "test-1")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer application.Close()
	root, err := application.Storage.Root()
	if err != nil {
		t.Fatal(err)
	}
	if root.Name != "Test Drive" {
		t.Fatalf("storage root name = %q", root.Name)
	}
}

func TestAssemblyFailureOrder(t *testing.T) {
	// The budget is built after the clients: a spool/limit failure must
	// propagate instead of leaking constructed transports.
	cfg := fixtureConfig(t)
	cfg.MaxUploads = 0
	if _, err := New(cfg, "test-1"); err == nil || err.Error() != "max_uploads must be positive" {
		t.Fatalf("budget failure = %v", err)
	}

	// The handlers are built last: a lock-limit failure still fails the
	// whole assembly.
	cfg = fixtureConfig(t)
	cfg.MaxLocks = 0
	if _, err := New(cfg, "test-1"); err == nil || err.Error() != "max_locks must be positive" {
		t.Fatalf("handler failure = %v", err)
	}

	// A broken workspace file fails during the state construction, before
	// any client exists.
	cfg = fixtureConfig(t)
	if err := os.WriteFile(cfg.WorkspaceFile, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, "test-1"); err == nil {
		t.Fatal("expected the broken workspace file to fail the assembly")
	}
}

func TestInlineCredentialsFillEmptyFields(t *testing.T) {
	// Inline-only setup: the source exists and returns the env values.
	cfg := fixtureConfig(t)
	cfg.InlineCookie = "wps_cookie=env-session; rtk=env-rtk; csrf=env-csrf"
	application, err := New(cfg, "test-1")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer application.Close()
	if application.Source == nil {
		t.Fatal("the inline values must produce a credential source")
	}
	snapshot, err := application.Source.Get()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Cookie != cfg.InlineCookie || snapshot.CSRFToken != "" {
		t.Fatalf("inline snapshot = %+v", snapshot)
	}
	if refreshed, err := application.Source.Refresh(); err != nil || refreshed {
		t.Fatalf("inline refresh = %v, %v", refreshed, err)
	}

	// File values win; inline values only fill empty fields, mirroring
	// client._credentials.
	cfg = withCredentials(t, fixtureConfig(t))
	cfg.CSRFTokenFile = "" // the file source reads an empty csrf
	if err := os.WriteFile(cfg.CookieFile, []byte("wps_cookie=file-session; rtk=file-rtk"), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err = New(cfg, "test-1")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer application.Close()
	snapshot, err = application.Source.Get()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot.Cookie, "file-session") || snapshot.CSRFToken != "" {
		t.Fatalf("mixed snapshot = %+v", snapshot)
	}
}

func TestCredentialReplacerWithoutFilesRefuses(t *testing.T) {
	replacer := credentialReplacer(fixtureConfig(t))
	replaced, err := replacer.ReplaceCredentials("cookie", "csrf")
	if replaced || err != nil {
		t.Fatalf("refusing replacer = %v, %v", replaced, err)
	}
}

func TestSessionImporterWiringFollowsWorkspace(t *testing.T) {
	// With a workspace state the roots surface exists; without one both
	// workspace surfaces stay nil, matching Python's set_root_id flow.
	cfg := withWorkspace(t, fixtureConfig(t), `{"group_id": "g", "root_id": "r"}`)
	application, err := New(cfg, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if application.roots() == nil || application.workspaceImporter() == nil {
		t.Fatal("workspace import surfaces must exist with a state")
	}

	// A pinned group and root with no file yield no workspace state, like
	// Python's from_env condition.
	cfg = fixtureConfig(t)
	cfg.GroupID = "pinned-group"
	cfg.RootID = "pinned-root"
	cfg.WorkspaceFile = filepath.Join(filepath.Dir(cfg.WorkspaceFile), "absent.json")
	application, err = New(cfg, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if application.roots() != nil || application.workspaceImporter() != nil {
		t.Fatal("workspace import surfaces must stay nil without a state")
	}
}

func TestSpaceFactorySharesResources(t *testing.T) {
	cfg := withWorkspace(t, fixtureConfig(t), `{"group_id": "g", "root_id": "r"}`)
	application, err := New(cfg, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()

	// The empty group reuses the base client.
	base, err := application.spaceFactory()("")
	if err != nil {
		t.Fatal(err)
	}
	if base.Lister != application.Client {
		t.Fatal("the empty group must reuse the base client")
	}

	// A mount builds its own client over the shared transports.
	mount, err := application.spaceFactory()("group-2")
	if err != nil {
		t.Fatal(err)
	}
	if mount.Lister == application.Client {
		t.Fatal("a mounted space must not reuse the base client")
	}
	if mount.Writer == nil || mount.Downloader == nil {
		t.Fatal("the mounted space surfaces are incomplete")
	}
}

func newTestServer(t *testing.T, cfg config.Config) (*httptest.Server, *Application) {
	t.Helper()
	application, err := New(cfg, "test-1")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(application.Close)
	handler, err := application.Handler()
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, application
}

func get(t *testing.T, url string, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

var authHeaders = map[string]string{"Authorization": "Basic " + basicAuth("adapter", "secret")}

// basicAuth encodes with the standard alphabet; kept dependency-free so the
// test cannot accidentally depend on another package's seam.
func basicAuth(username, password string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	data := []byte(username + ":" + password)
	var out strings.Builder
	for i := 0; i < len(data); i += 3 {
		var chunk [3]byte
		copy(chunk[:], data[i:])
		remaining := len(data) - i
		out.WriteByte(alphabet[chunk[0]>>2])
		out.WriteByte(alphabet[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if remaining == 1 {
			out.WriteByte('=')
		} else {
			out.WriteByte(alphabet[(chunk[1]&0x0F)<<2|chunk[2]>>6])
		}
		if remaining <= 2 {
			out.WriteByte('=')
		} else {
			out.WriteByte(alphabet[chunk[2]&0x3F])
		}
	}
	return out.String()
}

func TestWebRoutesRequireAuthentication(t *testing.T) {
	cfg := withAuth(t, fixtureConfig(t))
	server, _ := newTestServer(t, cfg)
	for _, path := range []string{"/", "/web", "/web/", "/assets/style.css", "/assets/app.js"} {
		response := get(t, server.URL+path, nil)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without auth = %d", path, response.StatusCode)
		}
		if challenge := response.Header.Get("WWW-Authenticate"); challenge != `Basic realm="wps-adapter"` {
			t.Errorf("%s challenge = %q", path, challenge)
		}
		if len(body) != 0 {
			t.Errorf("%s unauthorized body = %q", path, body)
		}
	}
	// /healthz stays public.
	response := get(t, server.URL+"/healthz", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", response.StatusCode)
	}
}

func TestWebPageEntriesServeFixedBytes(t *testing.T) {
	cfg := withAuth(t, fixtureConfig(t))
	server, _ := newTestServer(t, cfg)
	want := web.Page()
	for _, path := range []string{"/", "/web", "/web/"} {
		response := get(t, server.URL+path, authHeaders)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s = %d", path, response.StatusCode)
			continue
		}
		if string(body) != string(want) {
			t.Errorf("%s body differs from the embedded page", path)
		}
		if ct := response.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s Content-Type = %q", path, ct)
		}
		if cc := response.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s Cache-Control = %q", path, cc)
		}
		if csp := response.Header.Get("Content-Security-Policy"); csp != webContentSecurityPolicy {
			t.Errorf("%s CSP = %q", path, csp)
		}
		if sniff := response.Header.Get("X-Content-Type-Options"); sniff != "nosniff" {
			t.Errorf("%s X-Content-Type-Options = %q", path, sniff)
		}
		if cl := response.Header.Get("Content-Length"); cl == "" {
			t.Errorf("%s missing Content-Length", path)
		}
	}
	if !strings.Contains(string(want), "assets/style.css") || !strings.Contains(string(want), "assets/app.js") {
		t.Error("the page must link the split assets, not inline them")
	}
	if strings.Contains(string(want), "<script>") {
		t.Error("the page must not carry inline scripts")
	}
}

func embeddedAsset(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	data, contentType, ok := web.Asset(name)
	if !ok {
		t.Fatalf("asset %q missing from the embed manifest", name)
	}
	return data, contentType
}

func TestWebAssetsServeFixedBytes(t *testing.T) {
	cfg := withAuth(t, fixtureConfig(t))
	server, _ := newTestServer(t, cfg)
	for _, name := range []string{"style.css", "app.js"} {
		want, contentType := embeddedAsset(t, name)
		response := get(t, server.URL+"/assets/"+name, authHeaders)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s = %d", name, response.StatusCode)
			continue
		}
		if string(body) != string(want) {
			t.Errorf("%s body differs from the embedded asset", name)
		}
		if ct := response.Header.Get("Content-Type"); ct != contentType {
			t.Errorf("%s Content-Type = %q", name, ct)
		}
		if cc := response.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s Cache-Control = %q", name, cc)
		}
		if sniff := response.Header.Get("X-Content-Type-Options"); sniff != "nosniff" {
			t.Errorf("%s X-Content-Type-Options = %q", name, sniff)
		}
		if csp := response.Header.Get("Content-Security-Policy"); csp != "" {
			t.Errorf("%s unexpectedly carries a CSP", name)
		}
	}
}

func TestWebAssetUnknownNamesRefused(t *testing.T) {
	cfg := withAuth(t, fixtureConfig(t))
	server, _ := newTestServer(t, cfg)
	for _, path := range []string{
		"/assets/", "/assets/nope.js", "/assets/..%2Findex.html",
		"/assets/style.css%20", "/assets/../web/embed.go",
	} {
		response := get(t, server.URL+path, authHeaders)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d", path, response.StatusCode)
		}
		if string(body) != "unknown web asset\n" {
			t.Errorf("%s body = %q", path, body)
		}
		if response.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("%s Content-Type = %q", path, response.Header.Get("Content-Type"))
		}
	}
}

func TestWebAssetHeadHasNoBody(t *testing.T) {
	cfg := withAuth(t, fixtureConfig(t))
	server, _ := newTestServer(t, cfg)
	request, err := http.NewRequest(http.MethodHead, server.URL+"/assets/style.css", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", authHeaders["Authorization"])
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(body) != 0 {
		t.Errorf("HEAD asset = %d %q", response.StatusCode, body)
	}
	if response.Header.Get("Content-Length") == "" {
		t.Error("HEAD asset must still announce its length")
	}
}

func TestWebAssetOtherMethodsStayUnknownRoutes(t *testing.T) {
	cfg := withAuth(t, fixtureConfig(t))
	server, _ := newTestServer(t, cfg)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/assets/style.css", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", authHeaders["Authorization"])
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("POST asset = %d", response.StatusCode)
	}
}

func TestHealthzMatchesPythonContract(t *testing.T) {
	cfg := fixtureConfig(t)
	server, _ := newTestServer(t, cfg)
	response := get(t, server.URL+"/healthz", nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"status":"ok","service":"wps-enterprise-adapter","version":"test-1","network_calls":"on-demand"}`
	if string(body) != want {
		t.Errorf("healthz body = %q, want %q", body, want)
	}
	if ct := response.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := response.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func TestFallbackCredentialSourceWithoutFiles(t *testing.T) {
	source := &fallbackCredentialSource{cookie: "inline"}
	if refreshed, err := source.Refresh(); refreshed || err != nil {
		t.Fatalf("refresh without files = %v, %v", refreshed, err)
	}
	if stored, err := source.StoreSetCookieHeaders(nil); stored || err != nil {
		t.Fatalf("store without files = %v, %v", stored, err)
	}
	if replaced, err := source.ReplaceCredentials(credentials.Credentials{}); replaced || err != nil {
		t.Fatalf("replace without files = %v, %v", replaced, err)
	}
}

func TestHealthPayloadKeyOrder(t *testing.T) {
	payload, err := json.Marshal(healthPayload{Status: "ok", Service: "s", Version: "v", NetworkCalls: "on-demand"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"status":"ok","service":"s","version":"v","network_calls":"on-demand"}`
	if string(payload) != want {
		t.Fatalf("key order drifted: %s", payload)
	}
}
