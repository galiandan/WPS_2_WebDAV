package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		{"duplicate names", `{"spaces": [{"group_id": "g1", "name": "n"}, {"group_id": "g2", "name": "n"}]}`, "unique"},
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
