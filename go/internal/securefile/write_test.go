//go:build unix

package securefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomicCreatesPrivateFileWithTrailingNewline(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "state.json")
	mtime, err := WriteAtomic(path, `{"group_id":"g1"}`)
	if err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != `{"group_id":"g1"}`+"\n" {
		t.Errorf("content = %q, want payload plus one newline", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	if mtime != statMtime(t, path) {
		t.Errorf("mtime = %d, want the new file's mtime", mtime)
	}
}

func TestWriteAtomicReplacesExistingTarget(t *testing.T) {
	dir := privateDir(t)
	path := writePrivate(t, dir, "state.json", "old")
	if _, err := WriteAtomic(path, "new"); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "new\n" {
		t.Errorf("content = %q, want replaced", raw)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 after replace", info.Mode().Perm())
	}
	if leftovers := leftoverTemps(dir, "state.json"); len(leftovers) != 0 {
		t.Errorf("temp files survived: %v", leftovers)
	}
}

func TestWriteAtomicRejectsUnsafeParentsBeforeWriting(t *testing.T) {
	dir := privateDir(t)
	missingParent := filepath.Join(dir, "not-created", "state.json")
	if _, err := WriteAtomic(missingParent, "x"); CodeOf(err) != CodeTempCreate {
		t.Errorf("missing parent = %v, want temp_create (state writes fail like mkstemp)", err)
	}

	broad := filepath.Join(privateDir(t), "broad")
	if err := os.Mkdir(broad, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := WriteAtomic(filepath.Join(broad, "state.json"), "x"); CodeOf(err) != CodeParentUnsafe {
		t.Errorf("broad parent = %v, want parent_unsafe", err)
	}
}

func TestWriteAtomicTempCreateFailureLeavesTargetIntact(t *testing.T) {
	dir := privateDir(t)
	path := writePrivate(t, dir, "state.json", "old")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir readonly: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := WriteAtomic(path, "new")
	if CodeOf(err) != CodeTempCreate {
		t.Fatalf("WriteAtomic = %v, want temp_create", err)
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil || string(raw) != "old" {
		t.Errorf("target = %q (err %v), want the old content preserved", raw, readErr)
	}
	if leftovers := leftoverTemps(dir, "state.json"); len(leftovers) != 0 {
		t.Errorf("temp files survived: %v", leftovers)
	}
}

func TestWriteAtomicWriteStageFailureLeavesTargetIntact(t *testing.T) {
	dir := privateDir(t)
	path := writePrivate(t, dir, "state.json", "old")

	_, err := writeAtomicInto(path, "new", false, func(file *os.File, content string) error {
		if _, err := file.WriteString(content[:2]); err != nil {
			t.Fatalf("partial write: %v", err)
		}
		return errors.New("simulated mid-write failure")
	})
	if CodeOf(err) != CodeTempWrite {
		t.Fatalf("writeAtomicInto = %v, want temp_write", err)
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil || string(raw) != "old" {
		t.Errorf("target = %q (err %v), want the old content preserved", raw, readErr)
	}
	if leftovers := leftoverTemps(dir, "state.json"); len(leftovers) != 0 {
		t.Errorf("temp files survived: %v", leftovers)
	}
}

func TestWriteAtomicReplaceFailureCleansTemp(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "target.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	// A non-empty directory cannot be replaced by rename(2).
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	_, err := WriteAtomic(path, "new")
	if CodeOf(err) != CodeReplace {
		t.Fatalf("WriteAtomic = %v, want replace", err)
	}
	if leftovers := leftoverTemps(dir, "target.json"); len(leftovers) != 0 {
		t.Errorf("temp files survived: %v", leftovers)
	}
}

func TestWriteCredentialAtomicRequiresAndTightensParent(t *testing.T) {
	missing := filepath.Join(privateDir(t), "not-created", "cookie")
	if err := WriteCredentialAtomic(missing, "sid=abc"); CodeOf(err) != CodeParentUnavailable {
		t.Errorf("missing parent = %v, want parent_unavailable", err)
	}

	dir := privateDir(t)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := WriteCredentialAtomic(filepath.Join(dir, "cookie"), "sid=abc"); err != nil {
		t.Fatalf("WriteCredentialAtomic: %v", err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("parent mode = %v, want tightened to 0700", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "cookie"))
	if string(raw) != "sid=abc\n" {
		t.Errorf("cookie content = %q, want value plus newline", raw)
	}
}

func TestWriteCredentialPairUpdatesBothFiles(t *testing.T) {
	dir := privateDir(t)
	cookiePath := writePrivate(t, dir, "cookie", "sid=first")
	csrfPath := writePrivate(t, dir, "csrf", "csrf-first")

	if err := WriteCredentialPair(cookiePath, "sid=second", csrfPath, "csrf-second"); err != nil {
		t.Fatalf("WriteCredentialPair: %v", err)
	}
	cookie, _ := ReadSecret(cookiePath)
	csrf, _ := ReadSecret(csrfPath)
	if cookie != "sid=second" || csrf != "csrf-second" {
		t.Errorf("pair = (%q, %q), want both updated", cookie, csrf)
	}
}

func TestWriteCredentialPairRollsBackBothHalvesOnFailure(t *testing.T) {
	cookieDir := privateDir(t)
	csrfDir := privateDir(t)
	cookiePath := writePrivate(t, cookieDir, "cookie", "sid=first")
	if err := os.Chmod(csrfDir, 0o755); err != nil {
		t.Fatalf("chmod csrf dir broad: %v", err)
	}
	csrfPath := writePrivate(t, csrfDir, "csrf", "csrf-first")

	err := WriteCredentialPair(cookiePath, "sid=second", csrfPath, "csrf-second")
	if CodeOf(err) != CodeParentUnsafe {
		t.Fatalf("WriteCredentialPair = %v, want the csrf write failure", err)
	}
	cookie, cookieErr := ReadSecret(cookiePath)
	if cookieErr != nil || cookie != "sid=first" {
		t.Errorf("cookie = %q (err %v), want rolled back to sid=first", cookie, cookieErr)
	}
	csrfRaw, _ := os.ReadFile(csrfPath)
	if string(csrfRaw) != "csrf-first" {
		t.Errorf("csrf = %q, want untouched", csrfRaw)
	}
}

func TestWriteCredentialPairSnapshotFailureWritesNothing(t *testing.T) {
	dir := privateDir(t)
	cookiePath := writePrivate(t, dir, "cookie", "sid=first")
	csrfPath := filepath.Join(dir, "absent-csrf")

	if err := WriteCredentialPair(cookiePath, "sid=second", csrfPath, "csrf-second"); CodeOf(err) != CodeOpenMissing {
		t.Fatalf("WriteCredentialPair = %v, want snapshot failure", err)
	}
	raw, _ := os.ReadFile(cookiePath)
	if string(raw) != "sid=first" {
		t.Errorf("cookie = %q, want untouched", raw)
	}
}
