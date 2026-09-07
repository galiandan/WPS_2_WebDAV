//go:build unix

package securefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func privateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "securefile-test")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	return dir
}

func writePrivate(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod %s: %v", name, err)
	}
	return path
}

func wantCode(t *testing.T, err error, code Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %q, got nil", code)
	}
	if got := CodeOf(err); got != code {
		t.Fatalf("error code = %q, want %q (err: %v)", got, code, err)
	}
}

func TestReadJSONStateMissingFileAndParent(t *testing.T) {
	missingDir := filepath.Join(privateDir(t), "not-created")
	payload, mtime, err := ReadJSONState(filepath.Join(missingDir, "state.json"), 1024)
	if err != nil || payload != nil || mtime != nil {
		t.Errorf("missing parent: got (%v, %v, %v), want (nil, nil, nil)", payload, mtime, err)
	}

	dir := privateDir(t)
	payload, mtime, err = ReadJSONState(filepath.Join(dir, "state.json"), 1024)
	if err != nil || payload != nil || mtime != nil {
		t.Errorf("missing file: got (%v, %v, %v), want (nil, nil, nil)", payload, mtime, err)
	}
}

func TestReadSecretRequiresExistingParentAndStrips(t *testing.T) {
	missingDir := filepath.Join(privateDir(t), "not-created")
	_, err := ReadSecret(filepath.Join(missingDir, "cookie"))
	wantCode(t, err, CodeParentUnavailable)

	dir := privateDir(t)
	value, err := ReadSecret(writePrivate(t, dir, "cookie", "  sid=abc; csrf=x\n"))
	if err != nil {
		t.Fatalf("ReadSecret: %v", err)
	}
	if value != "sid=abc; csrf=x" {
		t.Errorf("ReadSecret = %q, want stripped value", value)
	}

	_, err = ReadSecret(filepath.Join(dir, "absent-cookie"))
	wantCode(t, err, CodeOpenMissing)
}

func TestReadJSONStateParsesObjectEmptyAndInvalidPayloads(t *testing.T) {
	dir := privateDir(t)
	path := writePrivate(t, dir, "state.json", `{"group_id":"g1","root_id":"r1"}`)
	payload, mtime, err := ReadJSONState(path, 16*1024)
	if err != nil {
		t.Fatalf("ReadJSONState: %v", err)
	}
	if payload["group_id"] != "g1" || payload["root_id"] != "r1" {
		t.Errorf("payload = %v, want group/root preserved", payload)
	}
	if mtime == nil || *mtime != statMtime(t, path) {
		t.Errorf("mtime = %v, want the file's mtime_ns", mtime)
	}

	empty := writePrivate(t, dir, "empty.json", "   \n\t")
	payload, mtime, err = ReadJSONState(empty, 16*1024)
	if err != nil || payload != nil || mtime == nil {
		t.Errorf("whitespace-only: got (%v, %v, %v), want (nil, mtime, nil)", payload, mtime, err)
	}

	array := writePrivate(t, dir, "array.json", `[]`)
	_, _, err = ReadJSONState(array, 16*1024)
	wantCode(t, err, CodeNotObject)

	broken := writePrivate(t, dir, "broken.json", `{"group_id":`)
	_, _, err = ReadJSONState(broken, 16*1024)
	wantCode(t, err, CodeNotJSON)
}

func statMtime(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return info.ModTime().UnixNano()
}

func TestReadJSONStateSizeLimit(t *testing.T) {
	dir := privateDir(t)
	exact := writePrivate(t, dir, "exact.json", `{"a":"`+strings.Repeat("a", 56)+`"}`)
	if _, _, err := ReadJSONState(exact, 64); err != nil {
		t.Errorf("file at the limit: %v", err)
	}
	over := writePrivate(t, dir, "over.json", `{"a":"`+strings.Repeat("a", 57)+`"}`)
	_, _, err := ReadJSONState(over, 64)
	wantCode(t, err, CodePostOpenUnsafe)
}

func TestReadJSONStateRejectsInvalidUTF8(t *testing.T) {
	dir := privateDir(t)
	path := writePrivate(t, dir, "bad-utf8.json", "{\"group_id\":\"\xff\xfe\"}")
	_, _, err := ReadJSONState(path, 16*1024)
	wantCode(t, err, CodeNotUTF8)
}

func TestReadSecretSizeBound(t *testing.T) {
	dir := privateDir(t)
	exact := writePrivate(t, dir, "exact", strings.Repeat("a", MaxCredentialFileBytes))
	value, err := ReadSecret(exact)
	if err != nil || len(value) != MaxCredentialFileBytes {
		t.Errorf("credential at the limit: len=%d err=%v", len(value), err)
	}
	over := writePrivate(t, dir, "over", strings.Repeat("a", MaxCredentialFileBytes+1))
	_, err = ReadSecret(over)
	wantCode(t, err, CodeTooLarge)
}

func TestCheckCredentialValues(t *testing.T) {
	if err := CheckCredentialValues("sid=abc", "csrf=xyz"); err != nil {
		t.Errorf("clean values: %v", err)
	}
	for _, bad := range []string{"a\x00b", "a\x7fb", "a\tb", "a\nb"} {
		if err := CheckCredentialValues(bad, ""); CodeOf(err) != CodeControlChar {
			t.Errorf("CheckCredentialValues(%q) = %v, want control character rejection", bad, err)
		}
	}
	oversized := strings.Repeat("a", MaxCredentialFileBytes+1)
	if err := CheckCredentialValues(oversized, ""); CodeOf(err) != CodeTooLarge {
		t.Errorf("oversized cookie = %v, want too large", err)
	}
}

func TestBroadPermissionsRejected(t *testing.T) {
	dir := privateDir(t)

	broadDir := filepath.Join(privateDir(t), "child")
	if err := os.Mkdir(broadDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, _, err := ReadJSONState(filepath.Join(broadDir, "state.json"), 1024)
	wantCode(t, err, CodeParentUnsafe)

	broadFile := writePrivate(t, dir, "state.json", "{}")
	if err := os.Chmod(broadFile, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, _, err = ReadJSONState(broadFile, 1024)
	wantCode(t, err, CodeFileUnsafe)

	broadSecret := writePrivate(t, dir, "cookie", "sid=abc")
	if err := os.Chmod(broadSecret, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err = ReadSecret(broadSecret)
	wantCode(t, err, CodePostOpenUnsafe)
}

func TestSymlinksRejected(t *testing.T) {
	dir := privateDir(t)
	target := writePrivate(t, dir, "real.json", `{"group_id":"g1"}`)

	link := filepath.Join(dir, "state-link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, _, err := ReadJSONState(link, 1024)
	wantCode(t, err, CodeNotRegular)

	secretLink := filepath.Join(dir, "cookie-link")
	if err := os.Symlink(target, secretLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err = ReadSecret(secretLink)
	wantCode(t, err, CodeOpenFailed)

	linkedParent := filepath.Join(dir, "dir-link")
	realDir := filepath.Join(dir, "real-dir")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(realDir, linkedParent); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, _, err = ReadJSONState(filepath.Join(linkedParent, "state.json"), 1024)
	wantCode(t, err, CodeParentSymlink)
}

func TestSymlinkedAncestorAboveMissingTailRejected(t *testing.T) {
	dir := privateDir(t)
	realDir := filepath.Join(dir, "real-dir")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "dir-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// The parent directory itself does not exist, so EvalSymlinks alone
	// could not resolve it; the component walk must still see the link.
	path := filepath.Join(link, "missing-sub", "state.json")
	_, _, err := ReadJSONState(path, 1024)
	wantCode(t, err, CodeParentSymlink)
}

func TestCheckPathShapeRules(t *testing.T) {
	dir := privateDir(t)

	if _, _, err := ReadJSONState(filepath.Join(dir, "a\nb.json"), 1024); CodeOf(err) != CodeInvalidPath {
		t.Errorf("state path with LF = %v, want invalid path", err)
	}
	// client.py rejects only NUL in credential paths; CR/LF just fail to open.
	_, err := ReadSecret(filepath.Join(dir, "a\nb"))
	if CodeOf(err) != CodeOpenMissing {
		t.Errorf("credential path with LF = %v, want open_missing", err)
	}
	if _, _, err := ReadJSONState("relative/state.json", 1024); CodeOf(err) != CodeInvalidPath {
		t.Errorf("relative path = %v, want invalid path", err)
	}
}

func TestCheckAfterOpenSeesModeChangedAfterOpen(t *testing.T) {
	dir := privateDir(t)
	path := writePrivate(t, dir, "cookie", "sid=abc")
	file, err := openSecure(path)
	if err != nil {
		t.Fatalf("openSecure: %v", err)
	}
	defer file.Close()
	if _, err := checkAfterOpen(file, 0); err != nil {
		t.Fatalf("checkAfterOpen on a private file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod after open: %v", err)
	}
	if _, err := checkAfterOpen(file, 0); CodeOf(err) != CodePostOpenUnsafe {
		t.Errorf("checkAfterOpen after chmod = %v, want post-open rejection", err)
	}
}
