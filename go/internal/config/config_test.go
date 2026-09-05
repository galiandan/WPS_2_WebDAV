package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allEnvNames lists every environment variable Load reads so tests run on a
// deterministic blank slate.
var allEnvNames = []string{
	"WPS_CREDENTIAL_REFRESH_COMMAND", "WPS_GROUP_ID", "WPS_ROOT_ID",
	"WPS_WORKSPACE_FILE", "WPS_COOKIE_FILE", "WPS_CSRF_TOKEN_FILE",
	"WPS_CREDENTIAL_REFRESH_TIMEOUT", "WPS_BASE_URL", "WPS_ACCOUNT_BASE_URL",
	"WPS_OBJECT_STORAGE_HOST_SUFFIX", "WPS_AUTO_REFRESH", "WPS_REFERER",
	"WPS_ORIGIN", "WPS_CID", "WPS_TIMEOUT", "WPS_STATUS_PROBE_TTL",
	"WPS_STATUS_FAILURE_BACKOFF", "WPS_UPLOAD_SPOOL_MEMORY",
	"WPS_STREAM_CHUNK_SIZE", "WPS_MULTIPART_THRESHOLD",
	"WPS_MULTIPART_PART_SIZE", "WPS_ENABLE_RANGE", "WPS_UPLOAD_SPOOL_DIR",
	"WPS_UPLOAD_RESUME_DIR", "WPS_UPLOAD_MIN_FREE_BYTES",
	"WPS_MAX_UPLOAD_BYTES", "WPS_UPLOAD_RETRIES", "WPS_UPLOAD_RETRY_DELAY",
	"WPS_MAX_JSON_RESPONSE_BYTES", "WPS_ROOT_NAME", "WPS_LIST_COUNT",
	"WPS_MAX_LIST_ENTRIES", "WPS_CACHE_TTL", "WPS_MAX_CACHED_FOLDERS",
	"WPS_MAX_UPLOADS", "WPS_MAX_DOWNLOADS", "WPS_TRANSFER_WAIT_TIMEOUT",
	"WPS_MAX_COPY_ENTRIES", "WPS_MAX_COPY_DEPTH", "WPS_MAX_PROPFIND_ENTRIES",
	"WPS_MAX_PROPFIND_DEPTH", "WPS_MAX_CONTROL_BODY",
	"WPS_MAX_RESPONSE_BODY_BYTES", "WPS_MAX_LOCKS",
	"ADAPTER_USERNAME", "ADAPTER_PASSWORD", "ADAPTER_USERNAME_FILE",
	"ADAPTER_PASSWORD_FILE", "ADAPTER_DAV_PREFIX", "ADAPTER_REST_PREFIX",
	"ADAPTER_BIND", "ADAPTER_PORT", "ADAPTER_MAX_CONNECTIONS",
	"ADAPTER_REQUEST_TIMEOUT",
}

// clearEnv unsets every configuration variable and points the workspace
// file at an absent temp path so tests are hermetic.

// mkPrivateDir creates a 0700 directory; t.TempDir() may be group-readable
// on some hosts, which the private-directory rules rightly reject.
func mkPrivateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wps-adapter-test")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func clearEnv(t *testing.T) {
	t.Helper()
	previous := map[string]string{}
	for _, name := range allEnvNames {
		if value, present := os.LookupEnv(name); present {
			previous[name] = value
		}
		os.Unsetenv(name)
	}
	t.Cleanup(func() {
		for _, name := range allEnvNames {
			os.Unsetenv(name)
		}
		for name, value := range previous {
			os.Setenv(name, value)
		}
	})
	fallbackWebSettingsFile = filepath.Join(mkPrivateDir(t), "web-settings.json")
	t.Cleanup(func() { fallbackWebSettingsFile = DefaultWebSettingsFile })
	workspaceFile := filepath.Join(mkPrivateDir(t), "wps-workspace.json")
	t.Setenv("WPS_WORKSPACE_FILE", workspaceFile)
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned %v", err)
	}
	if cfg.Bind != DefaultBind || cfg.Port != DefaultPort {
		t.Errorf("bind/port = %s/%d, want %s/%d", cfg.Bind, cfg.Port, DefaultBind, DefaultPort)
	}
	if cfg.DAVPrefix != "/dav" || cfg.RESTPrefix != "/api/v1" {
		t.Errorf("prefixes = %s/%s", cfg.DAVPrefix, cfg.RESTPrefix)
	}
	if cfg.BaseURL != "https://365.kdocs.cn" || cfg.ObjectSuffix != ".ag.kdocs.cn" {
		t.Errorf("upstream defaults drifted: %s %s", cfg.BaseURL, cfg.ObjectSuffix)
	}
	if !cfg.AutoRefresh || !cfg.EnableRange {
		t.Error("AutoRefresh/EnableRange should default to true")
	}
	if cfg.Timeout != 30 || cfg.StatusProbeTTL != 30 || cfg.StatusFailureBackup != 5 {
		t.Error("client timing defaults drifted")
	}
	if cfg.UploadSpoolMemory != 8*mib || cfg.StreamChunkSize != 1*mib {
		t.Error("spool defaults drifted")
	}
	if cfg.MultipartThreshold != 50*mib || cfg.MultipartPartSize != 10*mib {
		t.Error("multipart defaults drifted")
	}
	if cfg.UploadMinFreeBytes != 512*mib || cfg.MaxUploadBytes != 1*gib {
		t.Error("upload limit defaults drifted")
	}
	if cfg.UploadRetries != 2 || cfg.UploadRetryDelay != 0.5 {
		t.Error("retry defaults drifted")
	}
	if cfg.MaxJSONResponse != 8*mib {
		t.Error("JSON response cap drifted")
	}
	if cfg.RootName != DefaultRootName {
		t.Errorf("RootName = %q", cfg.RootName)
	}
	if cfg.ListCount != 20 || cfg.MaxListEntries != 10000 || cfg.CacheTTL != 2 {
		t.Error("storage defaults drifted")
	}
	if cfg.MaxCachedFolders != 1024 || cfg.MaxUploads != 2 || cfg.MaxDownloads != 4 {
		t.Error("storage limits drifted")
	}
	if cfg.TransferWait != 30 || cfg.MaxCopyEntries != 10000 || cfg.MaxCopyDepth != 64 {
		t.Error("copy/wait defaults drifted")
	}
	if cfg.MaxPropfindEntries != 10000 || cfg.MaxPropfindDepth != 64 {
		t.Error("propfind defaults drifted")
	}
	if cfg.MaxControlBody != 1*mib || cfg.MaxResponseBody != 16*mib || cfg.MaxLocks != 4096 {
		t.Error("application limit defaults drifted")
	}
	if cfg.MaxConnections != 64 || cfg.RequestTimeout != 60 {
		t.Error("server runtime defaults drifted")
	}
	if cfg.AuthEnabled() {
		t.Error("auth must be disabled without any credential setting")
	}
	// group "" auto-loads the absent workspace file: state exists with an
	// empty group and root "0".
	if cfg.WorkspaceState == nil {
		t.Fatal("workspace state should load for auto group ids")
	}
	if cfg.WorkspaceState.GroupID != "" || cfg.WorkspaceState.RootID != "0" {
		t.Errorf("workspace defaults drifted: %+v", cfg.WorkspaceState)
	}
	if cfg.ResolvedGroupID() != "" {
		t.Errorf("ResolvedGroupID = %q, want empty", cfg.ResolvedGroupID())
	}
}

func TestBooleanValues(t *testing.T) {
	for _, name := range []string{"WPS_AUTO_REFRESH", "WPS_ENABLE_RANGE"} {
		for _, text := range []string{"1", "true", "YES", "On", " true "} {
			clearEnv(t)
			t.Setenv(name, text)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("%s=%q: %v", name, text, err)
			}
			got := cfg.AutoRefresh
			if name == "WPS_ENABLE_RANGE" {
				got = cfg.EnableRange
			}
			if !got {
				t.Errorf("%s=%q: want true", name, text)
			}
		}
		for _, text := range []string{"0", "false", "NO", "Off", " FALSE "} {
			clearEnv(t)
			t.Setenv(name, text)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("%s=%q: %v", name, text, err)
			}
			got := cfg.AutoRefresh
			if name == "WPS_ENABLE_RANGE" {
				got = cfg.EnableRange
			}
			if got {
				t.Errorf("%s=%q: want false", name, text)
			}
		}
		for _, text := range []string{"2", "", "yes please", "wahr"} {
			clearEnv(t)
			t.Setenv(name, text)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: want boolean error naming the variable, got %v", name, text, err)
			}
		}
	}
}

func TestFloatParsingMatchesPythonFloat(t *testing.T) {
	cases := []struct {
		text    string
		wantNaN bool
		wantInf bool
		wantErr bool
	}{
		{text: "30"},
		{text: " 2.5 "},
		{text: "1e3"},
		{text: "1e999", wantInf: true},
		{text: "nan", wantNaN: true},
		{text: "abc", wantErr: true},
		{text: "", wantErr: true},
	}
	for _, tc := range cases {
		clearEnv(t)
		t.Setenv("WPS_TIMEOUT", tc.text)
		cfg, err := Load()
		if tc.wantErr {
			if err == nil {
				t.Errorf("WPS_TIMEOUT=%q: want error", tc.text)
			}
			continue
		}
		if err != nil {
			t.Fatalf("WPS_TIMEOUT=%q: %v", tc.text, err)
		}
		if tc.wantNaN && !math.IsNaN(cfg.Timeout) {
			t.Errorf("WPS_TIMEOUT=%q: want NaN", tc.text)
		}
		if tc.wantInf && !math.IsInf(cfg.Timeout, 1) {
			t.Errorf("WPS_TIMEOUT=%q: want +Inf", tc.text)
		}
	}
}

func TestIntegerParsing(t *testing.T) {
	cases := []struct {
		text    string
		want    int
		wantErr bool
	}{
		{text: "7", want: 7},
		{text: " 7 ", want: 7},
		{text: "+7", want: 7},
		{text: "99999999999999999999", wantErr: true},
		{text: "abc", wantErr: true},
		{text: "", wantErr: true},
	}
	for _, tc := range cases {
		clearEnv(t)
		t.Setenv("WPS_LIST_COUNT", tc.text)
		cfg, err := Load()
		if tc.wantErr {
			if err == nil {
				t.Errorf("WPS_LIST_COUNT=%q: want error", tc.text)
			}
			continue
		}
		if err != nil {
			t.Fatalf("WPS_LIST_COUNT=%q: %v", tc.text, err)
		}
		if cfg.ListCount != tc.want {
			t.Errorf("WPS_LIST_COUNT=%q: got %d", tc.text, cfg.ListCount)
		}
	}
}

func TestZeroAllowedVersusPositiveRequired(t *testing.T) {
	zeroAllowed := []string{
		"WPS_CACHE_TTL", "WPS_STATUS_PROBE_TTL", "WPS_STATUS_FAILURE_BACKOFF",
		"WPS_UPLOAD_MIN_FREE_BYTES", "WPS_MAX_UPLOAD_BYTES",
	}
	for _, name := range zeroAllowed {
		clearEnv(t)
		t.Setenv(name, "0")
		if _, err := Load(); err != nil {
			t.Errorf("%s=0 should be allowed, got %v", name, err)
		}
	}
	// A group must be configured so Python actually builds the storage whose
	// rules these are.
	clearEnv(t)
	t.Setenv("WPS_GROUP_ID", "bench-group")
	positiveRequired := []string{
		"WPS_LIST_COUNT", "WPS_MAX_LIST_ENTRIES", "WPS_MAX_CACHED_FOLDERS",
		"WPS_MAX_UPLOADS", "WPS_MAX_DOWNLOADS", "WPS_MAX_COPY_ENTRIES",
		"WPS_MAX_COPY_DEPTH", "WPS_MAX_PROPFIND_ENTRIES", "WPS_MAX_PROPFIND_DEPTH",
		"WPS_MAX_CONTROL_BODY", "WPS_MAX_RESPONSE_BODY_BYTES", "WPS_MAX_LOCKS",
		"WPS_MAX_JSON_RESPONSE_BYTES",
	}
	for _, name := range positiveRequired {
		for _, text := range []string{"0", "-1"} {
			clearEnv(t)
			t.Setenv("WPS_GROUP_ID", "bench-group")
			t.Setenv(name, text)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%s should fail naming the variable, got %v", name, text, err)
			}
		}
	}
	clearEnv(t)
	t.Setenv("WPS_GROUP_ID", "bench-group")
	t.Setenv("WPS_TRANSFER_WAIT_TIMEOUT", "0")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "WPS_TRANSFER_WAIT_TIMEOUT") {
		t.Errorf("zero transfer wait should fail, got %v", err)
	}
	// Python validates neither delay at load time; the client applies them
	// per upload.
	clearEnv(t)
	t.Setenv("WPS_UPLOAD_RETRY_DELAY", "-0.5")
	if _, err := Load(); err != nil {
		t.Errorf("negative retry delay has no load-time rule in Python, got %v", err)
	}
	// The client-side upload variables are likewise parse-only at load.
	for _, name := range []string{
		"WPS_UPLOAD_SPOOL_MEMORY", "WPS_STREAM_CHUNK_SIZE",
		"WPS_MULTIPART_THRESHOLD", "WPS_MULTIPART_PART_SIZE",
	} {
		clearEnv(t)
		t.Setenv(name, "-1")
		if _, err := Load(); err != nil {
			t.Errorf("%s=-1 has no load-time rule in Python, got %v", name, err)
		}
	}
}

func TestStorageValidationOnlyWithResolvedGroup(t *testing.T) {
	// Without a group, Python never constructs a WpsStorage, so a zero list
	// count stays unnoticed by check-config.
	clearEnv(t)
	t.Setenv("WPS_LIST_COUNT", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned %v", err)
	}
	if cfg.ListCount != 0 {
		t.Errorf("ListCount = %d", cfg.ListCount)
	}
	// With a group the same value fails.
	clearEnv(t)
	t.Setenv("WPS_GROUP_ID", "bench-group")
	t.Setenv("WPS_LIST_COUNT", "0")
	if _, err := Load(); err == nil {
		t.Error("WPS_LIST_COUNT=0 with a group should fail")
	}
}

func TestListCountMustNotExceedMaxListEntries(t *testing.T) {
	clearEnv(t)
	t.Setenv("WPS_GROUP_ID", "bench-group")
	t.Setenv("WPS_LIST_COUNT", "21")
	t.Setenv("WPS_MAX_LIST_ENTRIES", "20")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "WPS_LIST_COUNT") {
		t.Errorf("want ordering error, got %v", err)
	}
}

func TestBaseURLRules(t *testing.T) {
	accepted := []string{
		"https://365.kdocs.cn", "https://kdocs.cn", "https://KDOCS.CN",
		"https://365.kdocs.cn/", "https://365.kdocs.cn.", "https://a.b.kdocs.cn",
	}
	for _, text := range accepted {
		clearEnv(t)
		t.Setenv("WPS_BASE_URL", text)
		if _, err := Load(); err != nil {
			t.Errorf("WPS_BASE_URL=%q should pass, got %v", text, err)
		}
	}
	rejected := []string{
		"http://365.kdocs.cn", "https://evil.com", "https://u:p@365.kdocs.cn",
		"https://365.kdocs.cn/?x=1", "https://365.kdocs.cn/#f",
		"https://365.kdocs.cn/path", "365.kdocs.cn", "",
	}
	for _, text := range rejected {
		clearEnv(t)
		t.Setenv("WPS_BASE_URL", text)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "WPS_BASE_URL") {
			t.Errorf("WPS_BASE_URL=%q should fail naming the variable, got %v", text, err)
		}
	}
}

func TestObjectSuffixRules(t *testing.T) {
	accepted := []string{".ag.kdocs.cn", "ag.kdocs.cn", "kdocs.cn", ".KDOCS.CN"}
	for _, text := range accepted {
		clearEnv(t)
		t.Setenv("WPS_OBJECT_STORAGE_HOST_SUFFIX", text)
		if _, err := Load(); err != nil {
			t.Errorf("suffix %q should pass, got %v", text, err)
		}
	}
	rejected := []string{"evil.com", ".", "", "kdocs.com", "cn"}
	for _, text := range rejected {
		clearEnv(t)
		t.Setenv("WPS_OBJECT_STORAGE_HOST_SUFFIX", text)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "WPS_OBJECT_STORAGE_HOST_SUFFIX") {
			t.Errorf("suffix %q should fail, got %v", text, err)
		}
	}
}

func TestRootNameRules(t *testing.T) {
	clearEnv(t)
	t.Setenv("WPS_ROOT_NAME", "  ") // Python strips and rejects the residue.
	if _, err := Load(); err == nil {
		t.Error("whitespace root name should fail")
	}
	clearEnv(t)
	t.Setenv("WPS_ROOT_NAME", strings.Repeat("字", 300))
	if _, err := Load(); err == nil {
		t.Error("over-long root name should fail")
	}
	clearEnv(t)
	t.Setenv("WPS_ROOT_NAME", strings.Repeat("a", 256))
	if _, err := Load(); err != nil {
		t.Errorf("256-char root name should pass, got %v", err)
	}
	clearEnv(t)
	t.Setenv("WPS_ROOT_NAME", "云盘\x01")
	if _, err := Load(); err == nil {
		t.Error("control character in root name should fail")
	}
}

func TestPrefixNormalisation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/dav"}, {"dav", "/dav"}, {"/dav", "/dav"}, {"/dav/", "/dav"},
		{"//", "/"}, {"api/v1", "/api/v1"},
	}
	for _, tc := range cases {
		clearEnv(t)
		t.Setenv("ADAPTER_DAV_PREFIX", tc.in)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("prefix %q: %v", tc.in, err)
		}
		if cfg.DAVPrefix != tc.want {
			t.Errorf("prefix %q = %q, want %q", tc.in, cfg.DAVPrefix, tc.want)
		}
	}
}

func TestAuthEnabledUsesAnyRule(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		expect bool
	}{
		{"nothing", map[string]string{}, false},
		{"username only", map[string]string{"ADAPTER_USERNAME": "u"}, true},
		{"password only", map[string]string{"ADAPTER_PASSWORD": "p"}, true},
		{"username file only", map[string]string{"ADAPTER_USERNAME_FILE": "/tmp/u"}, true},
		{"password file only", map[string]string{"ADAPTER_PASSWORD_FILE": "/tmp/p"}, true},
	}
	for _, tc := range cases {
		clearEnv(t)
		for key, value := range tc.env {
			t.Setenv(key, value)
		}
		cfg, err := Load()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if cfg.AuthEnabled() != tc.expect {
			t.Errorf("%s: AuthEnabled = %v, want %v", tc.name, cfg.AuthEnabled(), tc.expect)
		}
	}
}

func TestRefreshCommandSplitting(t *testing.T) {
	clearEnv(t)
	t.Setenv("WPS_CREDENTIAL_REFRESH_COMMAND", `python3 /bin/refresh.py "a b" 'c'`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned %v", err)
	}
	want := []string{"python3", "/bin/refresh.py", "a b", "c"}
	if len(cfg.RefreshCommand) != len(want) {
		t.Fatalf("words = %q", cfg.RefreshCommand)
	}
	for i := range want {
		if cfg.RefreshCommand[i] != want[i] {
			t.Errorf("word %d = %q, want %q", i, cfg.RefreshCommand[i], want[i])
		}
	}
	clearEnv(t)
	t.Setenv("WPS_CREDENTIAL_REFRESH_COMMAND", `refresh "unbalanced`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "WPS_CREDENTIAL_REFRESH_COMMAND") {
		t.Errorf("unbalanced quote should fail, got %v", err)
	}
}

func TestWorkspaceLoadCondition(t *testing.T) {
	clearEnv(t)
	t.Setenv("WPS_GROUP_ID", "bench-group")
	t.Setenv("WPS_ROOT_ID", "bench-root")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned %v", err)
	}
	if cfg.WorkspaceState != nil {
		t.Error("explicit ids without a state file should not load the workspace")
	}
	// The Python service only validates the identifier when the workspace
	// loads; pin that quirk.
	clearEnv(t)
	t.Setenv("WPS_GROUP_ID", "not a valid id")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unpinned invalid group should pass without a state file: %v", err)
	}
	if cfg.ResolvedGroupID() != "not a valid id" {
		t.Errorf("ResolvedGroupID = %q", cfg.ResolvedGroupID())
	}
	// A present state file forces the workspace (and its validation) on.
	clearEnv(t)
	file := filepath.Join(mkPrivateDir(t), "workspace.json")
	os.WriteFile(file, []byte(`{"group_id": "gid-1"}`), 0o600)
	t.Setenv("WPS_GROUP_ID", "not a valid id")
	t.Setenv("WPS_WORKSPACE_FILE", file)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "WPS_GROUP_ID") {
		t.Errorf("existing state file should validate the configured id, got %v", err)
	}
}

func TestWorkspaceResolutionAndFileErrors(t *testing.T) {
	clearEnv(t)
	file := filepath.Join(mkPrivateDir(t), "workspace.json")
	os.WriteFile(file, []byte(`{"group_id": "gid-1", "root_id": "root-9"}`), 0o600)
	t.Setenv("WPS_WORKSPACE_FILE", file)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned %v", err)
	}
	if cfg.ResolvedGroupID() != "gid-1" {
		t.Errorf("ResolvedGroupID = %q", cfg.ResolvedGroupID())
	}
	// The configured root "0" is explicit, so the file root is ignored —
	// exactly like the Python resolution order.
	if cfg.resolvedRootID() != "0" {
		t.Errorf("root = %q, want 0", cfg.resolvedRootID())
	}
	clearEnv(t)
	file = filepath.Join(mkPrivateDir(t), "workspace.json")
	os.WriteFile(file, []byte(`{"root_id": "root-9"}`), 0o600)
	t.Setenv("WPS_WORKSPACE_FILE", file)
	t.Setenv("WPS_ROOT_ID", "auto")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() returned %v", err)
	}
	if cfg.resolvedRootID() != "root-9" {
		t.Errorf("auto root = %q, want root-9", cfg.resolvedRootID())
	}

	errorCases := []struct {
		name    string
		content string
		want    string
	}{
		{"broken json", "{oops", "workspace file is not valid JSON"},
		{"not an object", "[1]", "workspace file must contain a JSON object"},
		{"empty spaces", `{"spaces": []}`, "workspace.spaces is invalid"},
		{"duplicate groups", `{"spaces": [{"group_id": "g1"}, {"group_id": "g1"}]}`, "duplicate groups"},
		{"duplicate names", `{"spaces": [{"group_id": "g1", "name": "same"}, {"group_id": "g2", "name": "same"}]}`, "unique"},
		{"bad file group", `{"group_id": "bad id"}`, "workspace.group_id is invalid"},
		{"bad space name", `{"spaces": [{"group_id": "g1", "name": "a/b"}]}`, "space.name is invalid"},
	}
	for _, tc := range errorCases {
		clearEnv(t)
		file := filepath.Join(mkPrivateDir(t), "workspace.json")
		os.WriteFile(file, []byte(tc.content), 0o600)
		t.Setenv("WPS_WORKSPACE_FILE", file)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestPortParseIsGlobalWhileRangeIsServeOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADAPTER_PORT", "99999")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("check-config accepts an out-of-range port like Python: %v", err)
	}
	if err := cfg.ValidateRuntime(); err == nil {
		t.Error("serve path must reject the out-of-range port")
	}
	clearEnv(t)
	t.Setenv("ADAPTER_PORT", "abc")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ADAPTER_PORT") {
		t.Errorf("broken port must fail every command like Python, got %v", err)
	}
}

func TestServerRuntimeParsing(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADAPTER_MAX_CONNECTIONS", "0")
	t.Setenv("ADAPTER_REQUEST_TIMEOUT", "-1")
	maxConnections, requestTimeout, err := ParseServerRuntime()
	if err != nil {
		t.Fatalf("ParseServerRuntime returned %v", err)
	}
	if maxConnections != 0 || requestTimeout != -1 {
		t.Errorf("runtime = %d/%f", maxConnections, requestTimeout)
	}
	cfg := Config{Port: 1, MaxConnections: maxConnections, RequestTimeout: requestTimeout}
	if err := cfg.ValidateRuntime(); err == nil {
		t.Error("ValidateRuntime must reject non-positive runtime values")
	}
	clearEnv(t)
	t.Setenv("ADAPTER_MAX_CONNECTIONS", "abc")
	if _, _, err := ParseServerRuntime(); err == nil || !strings.Contains(err.Error(), "ADAPTER_MAX_CONNECTIONS") {
		t.Errorf("broken max connections must fail, got %v", err)
	}
}

func TestErrorOrderMatchesPython(t *testing.T) {
	// The workspace file is read before the storage integers are parsed, so
	// its error wins.
	clearEnv(t)
	file := filepath.Join(mkPrivateDir(t), "workspace.json")
	os.WriteFile(file, []byte("{broken"), 0o600)
	t.Setenv("WPS_WORKSPACE_FILE", file)
	t.Setenv("WPS_LIST_COUNT", "abc")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "workspace file") {
		t.Errorf("workspace error should come first, got %v", err)
	}
}
