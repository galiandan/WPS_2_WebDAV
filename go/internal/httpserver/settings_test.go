package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

// recordingCredentialReplacer refuses every import; the settings tests
// never reach it.
type recordingCredentialReplacer struct{}

func (r *recordingCredentialReplacer) ReplaceCredentials(string, string) (bool, error) {
	return false, nil
}

// settingsRouter builds a router with the REST dispatcher wired for the
// settings routes and a recording storage.
type recordingRootNameStorage struct {
	mu       sync.Mutex
	names    []string
	failNext bool
}

func (s *recordingRootNameStorage) SetRootName(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext {
		s.failNext = false
		return errBadRequest("storage rejected the name")
	}
	s.names = append(s.names, name)
	return nil
}

func (s *recordingRootNameStorage) applied() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

type settingsHarness struct {
	router   *Router
	storage  *recordingRootNameStorage
	settings *workspace.WebSettings
	path     string
}

func newSettingsHarness(t *testing.T) *settingsHarness {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "web-settings.json")
	settings, err := workspace.NewWebSettings(path, workspace.DefaultRootName)
	if err != nil {
		t.Fatal(err)
	}
	storage := &recordingRootNameStorage{}
	controller, err := NewRootNameController(settings, storage)
	if err != nil {
		t.Fatal(err)
	}
	limits := ControlLimits{MaxControlBody: 1024, MaxResponseBody: 4096}
	session, err := NewSessionImporter(limits, "",
		func([]any, string) (string, string, []string, error) {
			return "", "", nil, errBadRequest("unused in settings tests")
		},
		&recordingCredentialReplacer{}, nil, nil)
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
	return &settingsHarness{router: router, storage: storage, settings: settings, path: path}
}

func (h *settingsHarness) request(t *testing.T, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	return recorder
}

func jsonRequestHeaders(body string) map[string]string {
	return map[string]string{
		"Content-Type":   "application/json",
		"Content-Length": strconv.Itoa(len(body)),
	}
}

// TestSettingsGETReturnsHotName pins GET /api/v1/settings: the current hot
// name, hot reload after an external write, and body tolerance.
func TestSettingsGETReturnsHotName(t *testing.T) {
	harness := newSettingsHarness(t)

	recorder := harness.request(t, "GET", "/api/v1/settings", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != `{"status":"ok","name":"WPS Enterprise Drive"}` {
		t.Errorf("body = %q", body)
	}

	// An external writer changes the file; the next GET hot-reloads.
	if err := os.WriteFile(harness.path, []byte(`{"name":"外部改名"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder = harness.request(t, "GET", "/api/v1/settings", "", nil)
	if body := recorder.Body.String(); body != `{"status":"ok","name":"\u5916\u90e8\u6539\u540d"}` {
		t.Errorf("hot-reloaded body = %q", body)
	}
	// The hot change propagated into the storage too.
	if applied := harness.storage.applied(); len(applied) != 2 || applied[1] != "外部改名" {
		t.Errorf("storage names = %q", applied)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
}

// TestSettingsGETToleratesAndRejectsBodies mirrors the discard step: small
// bodies are consumed, oversized ones answer 413.
func TestSettingsGETToleratesAndRejectsBodies(t *testing.T) {
	harness := newSettingsHarness(t)

	recorder := harness.request(t, "GET", "/api/v1/settings", "ignored-body", jsonRequestHeaders("ignored-body"))
	if recorder.Code != http.StatusOK {
		t.Errorf("small GET body broke settings: %d", recorder.Code)
	}

	big := strings.Repeat("x", 2048) // above the 1024 control-body cap
	recorder = harness.request(t, "GET", "/api/v1/settings", big, jsonRequestHeaders(big))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized GET body status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"error":"request body is too large"}` {
		t.Errorf("oversized GET body response = %q", body)
	}
}

// TestSettingsPATCHLifecycle pins the PATCH contract: exactly the name
// field, validation errors, persistence to the fixture, and storage
// propagation without any restart.
func TestSettingsPATCHLifecycle(t *testing.T) {
	harness := newSettingsHarness(t)

	recorder := harness.request(t, "PATCH", "/api/v1/settings", `{"name":"我的云盘"}`, jsonRequestHeaders(`{"name":"我的云盘"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != `{"status":"ok","name":"\u6211\u7684\u4e91\u76d8"}` {
		t.Errorf("body = %q", body)
	}
	raw, err := os.ReadFile(harness.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"name":"\u6211\u7684\u4e91\u76d8"}`+"\n" {
		t.Errorf("fixture = %q, want Python-format payload", raw)
	}
	if applied := harness.storage.applied(); len(applied) != 2 || applied[1] != "我的云盘" {
		t.Errorf("storage names = %q", applied)
	}

	// The GET now reports the persisted name without any further write.
	recorder = harness.request(t, "GET", "/api/v1/settings", "", nil)
	if body := recorder.Body.String(); body != `{"status":"ok","name":"\u6211\u7684\u4e91\u76d8"}` {
		t.Errorf("GET after PATCH = %q", body)
	}
}

// TestSettingsPATCHValidation pins the body and value rules.
func TestSettingsPATCHValidation(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantCode  int
		wantBody  string
		wantClose bool
	}{
		{
			name:     "extra field",
			body:     `{"name":"x","extra":1}`,
			wantCode: http.StatusBadRequest,
			wantBody: `{"error":"JSON field 'name' is required"}`,
		},
		{
			name:     "missing name field",
			body:     `{"other":1}`,
			wantCode: http.StatusBadRequest,
			wantBody: `{"error":"JSON field 'name' is required"}`,
		},
		{
			name:     "empty object",
			body:     `{}`,
			wantCode: http.StatusBadRequest,
			wantBody: `{"error":"JSON field 'name' is required"}`,
		},
		{
			name:     "non-string name",
			body:     `{"name":42}`,
			wantCode: http.StatusBadRequest,
			wantBody: `{"error":"root name must be a string"}`,
		},
		{
			name:     "empty name",
			body:     `{"name":"   "}`,
			wantCode: http.StatusBadRequest,
			wantBody: `{"error":"root name must not be empty"}`,
		},
		{
			name:     "too long name",
			body:     `{"name":"` + strings.Repeat("长", 300) + `"}`,
			wantCode: http.StatusBadRequest,
			wantBody: `{"error":"root name is too long"}`,
		},
		{
			name:     "control character",
			body:     `{"name":"bad\u0001name"}`,
			wantCode: http.StatusBadRequest,
			wantBody: `{"error":"root name contains a control character"}`,
		},
		{
			name:      "invalid json",
			body:      `{nope}`,
			wantCode:  http.StatusBadRequest,
			wantBody:  `{"error":"request body must be valid JSON"}`,
			wantClose: false,
		},
		{
			name:      "json array",
			body:      `[1,2]`,
			wantCode:  http.StatusBadRequest,
			wantBody:  `{"error":"request body must be a JSON object"}`,
			wantClose: false,
		},
		{
			name:      "short body",
			body:      `{"name":"x"}`,
			wantCode:  http.StatusBadRequest,
			wantBody:  `{"error":"request body is shorter than Content-Length"}`,
			wantClose: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newSettingsHarness(t)
			headers := jsonRequestHeaders(tc.body)
			if tc.name == "short body" {
				headers["Content-Length"] = "100"
			}
			recorder := harness.request(t, "PATCH", "/api/v1/settings", tc.body, headers)
			if recorder.Code != tc.wantCode {
				t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
			}
			if body := recorder.Body.String(); body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			if got := recorder.Header().Get("Connection"); (got == "close") != tc.wantClose {
				t.Errorf("Connection = %q, wantClose %v", got, tc.wantClose)
			}
		})
	}
}

// TestSettingsPatchRequiresContentLength pins the 411: no Content-Length
// answers the text-format LENGTH_REQUIRED, not a JSON error.
func TestSettingsPatchRequiresContentLength(t *testing.T) {
	harness := newSettingsHarness(t)
	request := httptest.NewRequest("PATCH", "/api/v1/settings", strings.NewReader(`{"name":"x"}`))
	recorder := httptest.NewRecorder()
	harness.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusLengthRequired {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != "Content-Length is required\n" {
		t.Errorf("body = %q", body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Connection"); got != "close" {
		t.Errorf("Connection = %q", got)
	}
}

// TestSettingsNameInteroperatesWithPython fulfils the B603 completion
// condition: Python and Go take turns reading and writing one fixture file.
func TestSettingsNameInteroperatesWithPython(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "src", "wps_adapter", "settings.py")); err != nil {
		t.Skip("Python reference tree is not available")
	}

	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "web-settings.json")

	runPython := func(script string, args ...string) string {
		t.Helper()
		cmd := exec.Command(python, append([]string{"-c", script}, args...)...)
		cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(repoRoot, "src"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("python failed: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	goSettings, err := workspace.NewWebSettings(fixture, "Go 缺省")
	if err != nil {
		t.Fatal(err)
	}

	// Round 1: Python writes, Go hot-reads.
	runPython(`import sys
from wps_adapter.settings import WebSettings
settings = WebSettings(sys.argv[1], fallback_name="ignored")
settings.set_name("来自 Python")`, fixture)
	name, err := goSettings.Name()
	if err != nil {
		t.Fatal(err)
	}
	if name != "来自 Python" {
		t.Fatalf("Go read %q after the Python write", name)
	}

	// Round 2: Go writes, Python reads.
	if _, err := goSettings.SetName("来自 Go"); err != nil {
		t.Fatal(err)
	}
	got := runPython(`import sys
from wps_adapter.settings import WebSettings
print(WebSettings(sys.argv[1]).name)`, fixture)
	if got != "来自 Go" {
		t.Fatalf("Python read %q after the Go write", got)
	}

	// The fixture stays byte-identical to Python's persisted format.
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"name":"\u6765\u81ea Go"}`+"\n" {
		t.Errorf("fixture = %q", raw)
	}
}

// TestSettingsConcurrentReadWrite survives the race detector: hot reads and
// writes race through the dispatcher without torn state.
func TestSettingsConcurrentReadWrite(t *testing.T) {
	harness := newSettingsHarness(t)

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				if worker%2 == 0 {
					recorder := harness.request(t, "GET", "/api/v1/settings", "", nil)
					if recorder.Code != http.StatusOK {
						t.Errorf("concurrent GET failed: %d %q", recorder.Code, recorder.Body.String())
						return
					}
					continue
				}
				body := `{"name":"并发-` + strconv.Itoa(worker) + `-` + strconv.Itoa(round) + `"}`
				recorder := harness.request(t, "PATCH", "/api/v1/settings", body, jsonRequestHeaders(body))
				if recorder.Code != http.StatusOK {
					t.Errorf("concurrent PATCH failed: %d %q", recorder.Code, recorder.Body.String())
					return
				}
			}
		}(worker)
	}
	wg.Wait()

	// The fixture parses and holds the last written name.
	raw, err := os.ReadFile(harness.path)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("fixture corrupted: %v (%q)", err, raw)
	}
	if _, ok := payload["name"]; !ok {
		t.Errorf("fixture lost the name field: %q", raw)
	}
}
