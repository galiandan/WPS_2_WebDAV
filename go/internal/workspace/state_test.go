package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkPrivateDir creates a 0700 directory; t.TempDir() may be group-readable,
// which the private-directory rules rightly reject.
func mkPrivateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "workspace-test")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestValidateIdentifier(t *testing.T) {
	accepted := []string{"g", "bench-group", "a.b", "x_y", "Z-9", strings.Repeat("a", 256)}
	for _, value := range accepted {
		if err := ValidateIdentifier(value, "field"); err != nil {
			t.Errorf("%q should pass, got %v", value, err)
		}
	}
	rejected := []string{"", "a b", "a/b", "a\\b", "云", strings.Repeat("a", 257)}
	for _, value := range rejected {
		if err := ValidateIdentifier(value, "field"); err == nil {
			t.Errorf("%q should be rejected", value)
		}
	}
}

func TestLoadFromFileWithoutPayload(t *testing.T) {
	dir := mkPrivateDir(t)
	state, err := LoadFromFile(filepath.Join(dir, "absent.json"), "", "0")
	if err != nil {
		t.Fatalf("LoadFromFile returned %v", err)
	}
	if state.GroupID != "" || state.RootID != "0" || state.Spaces != nil {
		t.Errorf("state = %+v", state)
	}
}

func TestResolutionOrder(t *testing.T) {
	dir := mkPrivateDir(t)
	file := filepath.Join(dir, "workspace.json")
	payload := `{"group_id": "file-group", "root_id": "file-root"}`
	if err := os.WriteFile(file, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	// Auto group/root adopt the file values.
	state, err := LoadFromFile(file, "", AutoValue)
	if err != nil {
		t.Fatalf("LoadFromFile returned %v", err)
	}
	if state.GroupID != "file-group" || state.RootID != "file-root" {
		t.Errorf("auto resolution = %+v", state)
	}

	// Explicit values win over the file.
	state, err = LoadFromFile(file, "explicit-group", "explicit-root")
	if err != nil {
		t.Fatalf("LoadFromFile returned %v", err)
	}
	if state.GroupID != "explicit-group" || state.RootID != "explicit-root" {
		t.Errorf("explicit resolution = %+v", state)
	}
}

func TestFileValidation(t *testing.T) {
	dir := mkPrivateDir(t)
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"broken json", "{oops", "workspace file is not valid JSON"},
		{"array payload", "[]", "workspace file must contain a JSON object"},
		{"empty spaces", `{"spaces": []}`, "workspace.spaces is invalid"},
		{"too many spaces", `{"spaces": [` + strings.TrimSuffix(strings.Repeat(`{"group_id": "g"},`, MaxSpaces+1), ",") + `]}`, "workspace.spaces is invalid"},
		{"duplicate groups", `{"spaces": [{"group_id": "g1"}, {"group_id": "g1"}]}`, "duplicate groups"},
		{"bad file group", `{"group_id": "bad id"}`, "workspace.group_id is invalid"},
		{"bad file root", `{"root_id": "bad/root"}`, "workspace.root_id is invalid"},
		{"bad space group", `{"spaces": [{"group_id": ""}]}`, "space.group_id is invalid"},
		{"empty space name", `{"spaces": [{"group_id": "g1", "name": ""}]}`, "space.name is invalid"},
		{"name with slash", `{"spaces": [{"group_id": "g1", "name": "a/b"}]}`, "space.name is invalid"},
	}
	for _, tc := range cases {
		file := filepath.Join(dir, "workspace.json")
		if err := os.WriteFile(file, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFromFile(file, "", "0"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", tc.name, tc.want, err)
		}
	}
}

// Duplicate mount names are accepted at load time like the Python loader;
// MultiSpaceStorage enforces uniqueness when the spaces are mounted.
func TestDuplicateSpaceNamesLoadLikePython(t *testing.T) {
	dir := mkPrivateDir(t)
	file := filepath.Join(dir, "workspace.json")
	payload := `{"spaces": [{"group_id": "g1", "name": "n"}, {"group_id": "g2", "name": "n"}]}`
	if err := os.WriteFile(file, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := LoadFromFile(file, "", "0")
	if err != nil {
		t.Fatalf("LoadFromFile returned %v", err)
	}
	if len(state.Spaces) != 2 {
		t.Errorf("spaces = %+v, want both duplicates loaded", state.Spaces)
	}
}

func TestPathValidation(t *testing.T) {
	dir := mkPrivateDir(t)
	if _, err := LoadFromFile("relative.json", "", "0"); err == nil {
		t.Error("relative path should be rejected")
	}
	// A group-readable directory is not private.
	openDir, err := os.MkdirTemp("", "workspace-open")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(openDir)
	if err := os.Chmod(openDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFile(filepath.Join(openDir, "w.json"), "", "0"); err == nil {
		t.Error("group-readable parent should be rejected")
	}
	// A symlinked parent is rejected.
	linkDir := filepath.Join(dir, "link")
	if err := os.Symlink(dir, linkDir); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFile(filepath.Join(linkDir, "w.json"), "", "0"); err == nil {
		t.Error("symlinked parent should be rejected")
	}
	// A broad-permission file is rejected.
	loose := filepath.Join(dir, "loose.json")
	if err := os.WriteFile(loose, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFile(loose, "", "0"); err == nil {
		t.Error("0644 workspace file should be rejected")
	}
	// An oversized file is rejected.
	huge := filepath.Join(dir, "huge.json")
	if err := os.WriteFile(huge, make([]byte, MaxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFile(huge, "", "0"); err == nil {
		t.Error("oversized workspace file should be rejected")
	}
}

func writeWorkspaceFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// bumpMtime forces a distinct mtime so reload tests do not depend on
// filesystem timestamp granularity.
func bumpMtime(t *testing.T, path string) {
	t.Helper()
	stamp := statMtime(t, path).Add(2 * time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}

func statMtime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return info.ModTime()
}

func TestPendingLoginWhenFileMissing(t *testing.T) {
	dir := mkPrivateDir(t)
	state, err := NewWorkspaceState(filepath.Join(dir, "absent.json"), "", AutoValue)
	if err != nil {
		t.Fatalf("NewWorkspaceState: %v", err)
	}
	groupID, err := state.GroupID()
	if err != nil || groupID != "" {
		t.Errorf("GroupID = (%q, %v), want pending empty", groupID, err)
	}
	rootID, err := state.RootID()
	if err != nil || rootID != "0" {
		t.Errorf("RootID = (%q, %v), want 0", rootID, err)
	}
	configured, err := state.Configured()
	if err != nil || configured {
		t.Errorf("Configured = (%v, %v), want false", configured, err)
	}
}

func TestHotReloadAppliesFileChangesAtomically(t *testing.T) {
	dir := mkPrivateDir(t)
	file := filepath.Join(dir, "workspace.json")
	writeWorkspaceFile(t, file, `{"group_id": "g1", "root_id": "r1"}`)
	state, err := NewWorkspaceState(file, "", AutoValue)
	if err != nil {
		t.Fatalf("NewWorkspaceState: %v", err)
	}

	// A broken rewrite must not disturb the previously applied state:
	// every access re-reads (like Python) and errors until the file is
	// valid again, while the in-memory values stay intact.
	writeWorkspaceFile(t, file, `{"spaces": []}`)
	bumpMtime(t, file)
	if _, err := state.GroupID(); err == nil {
		t.Error("expected the broken file to raise on access")
	}
	if _, err := state.GroupID(); err == nil || err.Error() != "workspace.spaces is invalid" {
		t.Errorf("repeated access = %v, want the same reload error", err)
	}
	state.mu.Lock()
	if state.groupID != "g1" || state.spaces != nil || state.rootID != "r1" {
		t.Errorf("state after failed reload = (%q, %q, %+v), want the previous values",
			state.groupID, state.rootID, state.spaces)
	}
	state.mu.Unlock()

	// A valid rewrite is adopted on the next access.
	writeWorkspaceFile(t, file, `{"group_id": "g2", "root_id": "r2"}`)
	bumpMtime(t, file)
	groupID, err := state.GroupID()
	if err != nil || groupID != "g2" {
		t.Errorf("GroupID after rewrite = (%q, %v), want g2", groupID, err)
	}
	rootID, err := state.RootID()
	if err != nil || rootID != "r2" {
		t.Errorf("RootID after rewrite = (%q, %v), want r2", rootID, err)
	}

	// Deleting the file returns the auto state to pending login.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	groupID, err = state.GroupID()
	if err != nil || groupID != "" {
		t.Errorf("GroupID after delete = (%q, %v), want pending empty", groupID, err)
	}
}

func TestUpdatePersistsAndAdopts(t *testing.T) {
	dir := mkPrivateDir(t)
	file := filepath.Join(dir, "workspace.json")
	state, err := NewWorkspaceState(file, "", AutoValue)
	if err != nil {
		t.Fatalf("NewWorkspaceState: %v", err)
	}
	mount, err := NewMount("g1", "r1", "空间一")
	if err != nil {
		t.Fatalf("NewMount: %v", err)
	}
	if err := state.Update("g1", "r1", []Mount{mount}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Byte-exact persist contract: compact separators, ensure_ascii, "\n".
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"group_id\":\"g1\",\"root_id\":\"r1\"," +
		`"spaces":[{"group_id":"g1","root_id":"r1","name":"\u7a7a\u95f4\u4e00"}]}` + "\n"
	if string(raw) != want {
		t.Errorf("persisted = %q, want %q", raw, want)
	}

	groupID, err := state.GroupID()
	if err != nil || groupID != "g1" {
		t.Errorf("GroupID after Update = (%q, %v), want g1", groupID, err)
	}
	spaces, err := state.Spaces()
	if err != nil || len(spaces) != 1 || spaces[0].Name != "空间一" {
		t.Errorf("Spaces after Update = %+v (err %v)", spaces, err)
	}

	// Configured (non-auto) values are never overwritten by Update.
	fixed, err := NewWorkspaceState(file, "fixed-group", "fixed-root")
	if err != nil {
		t.Fatalf("NewWorkspaceState fixed: %v", err)
	}
	if err := fixed.Update("other", "other-root", nil); err != nil {
		t.Fatalf("Update fixed: %v", err)
	}
	groupID, err = fixed.GroupID()
	if err != nil || groupID != "fixed-group" {
		t.Errorf("fixed group = (%q, %v), want fixed-group", groupID, err)
	}
	rootID, err := fixed.RootID()
	if err != nil || rootID != "fixed-root" {
		t.Errorf("fixed root = (%q, %v), want fixed-root", rootID, err)
	}
	spaces, err = fixed.Spaces()
	if err != nil || spaces != nil {
		t.Errorf("fixed spaces = %+v, want cleared", spaces)
	}
}

func TestPersistedFileRoundTripsThroughLoader(t *testing.T) {
	dir := mkPrivateDir(t)
	file := filepath.Join(dir, "workspace.json")
	state, err := NewWorkspaceState(file, "", AutoValue)
	if err != nil {
		t.Fatalf("NewWorkspaceState: %v", err)
	}
	mount, err := NewMount("g1", "r1", "drive-a")
	if err != nil {
		t.Fatalf("NewMount: %v", err)
	}
	if err := state.Update("g1", "r1", []Mount{mount}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// A fresh instance reads the Go-written file exactly like the Python
	// service would.
	reloaded, err := NewWorkspaceState(file, "", AutoValue)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	groupID, err := reloaded.GroupID()
	if err != nil || groupID != "g1" {
		t.Errorf("reloaded group = (%q, %v), want g1", groupID, err)
	}
	spaces, err := reloaded.Spaces()
	if err != nil || len(spaces) != 1 || spaces[0] != mount {
		t.Errorf("reloaded spaces = %+v (err %v), want one mount", spaces, err)
	}
}

func TestFilePayloadValidation(t *testing.T) {
	dir := mkPrivateDir(t)
	file := filepath.Join(dir, "workspace.json")

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"numeric group", `{"group_id": 123}`, "workspace.group_id is invalid"},
		{"null group", `{"group_id": null}`, "workspace.group_id is invalid"},
		{"numeric root", `{"root_id": 0}`, "workspace.root_id is invalid"},
		{"null root", `{"root_id": null}`, "workspace.root_id is invalid"},
		{"numeric space name", `{"spaces": [{"group_id": "g1", "name": 3}]}`, "space.name is invalid"},
		{"name with control char", `{"spaces": [{"group_id": "g1", "name": "a\u0007b"}]}`, "space.name is invalid"},
		{"name with DEL", "{\"spaces\": [{\"group_id\": \"g1\", \"name\": \"a\u007fb\"}]}", "space.name is invalid"},
		{"bad space root", `{"spaces": [{"group_id": "g1", "root_id": "x/y"}]}`, "space.root_id is invalid"},
		{"non-object space", `{"spaces": ["g1"]}`, "workspace space is invalid"},
	}
	for _, tc := range cases {
		writeWorkspaceFile(t, file, tc.content)
		if _, err := LoadFromFile(file, "", "0"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", tc.name, tc.want, err)
		}
	}

	// Invalid UTF-8 is reported before JSON parsing, like Python.
	if err := os.WriteFile(file, []byte("{\"group_id\":\"\xff\xfe\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFile(file, "", "0"); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Errorf("invalid UTF-8: got %v", err)
	}
}

func TestPythonWrittenFileIsReadable(t *testing.T) {
	dir := mkPrivateDir(t)
	file := filepath.Join(dir, "workspace.json")
	// Byte shape json.dump(..., ensure_ascii=True, separators=(",", ":")) writes.
	writeWorkspaceFile(t, file,
		"{\"group_id\": \"g1\", \"root_id\": \"r1\", \"spaces\": [{\"group_id\": \"g1\", \"root_id\": \"r1\", \"name\": \"\\u4e91\\u76d8\"}]}\n")
	state, err := LoadFromFile(file, "", "0")
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if len(state.Spaces) != 1 || state.Spaces[0].Name != "云盘" {
		t.Errorf("spaces = %+v, want the decoded name", state.Spaces)
	}
}

func TestPyEscapeMatchesEnsureASCII(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"plain", "plain"},
		{`a"b`, `a\"b`},
		{`a\b`, `a\\b`},
		{"a\nb", `a\nb`},
		{"a\tb", `a\tb`},
		{"a\x7fb", `a\u007fb`},
		{"a\x01b", `a\u0001b`},
		{"\u4e91\u76d8", `\u4e91\u76d8`},
		{"\U0001F600", `\ud83d\ude00`},
		{"~", "~"},
	}
	for _, tc := range cases {
		if got := pyEscape(tc.input); got != tc.want {
			t.Errorf("pyEscape(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
