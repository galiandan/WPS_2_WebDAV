package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebSettingsFallsBackAndSurvivesRestart(t *testing.T) {
	dir := mkPrivateDir(t)
	file := filepath.Join(dir, "web-settings.json")
	settings, err := NewWebSettings(file, "初始云盘")
	if err != nil {
		t.Fatalf("NewWebSettings: %v", err)
	}
	name, err := settings.Name()
	if err != nil || name != "初始云盘" {
		t.Errorf("Name = (%q, %v), want fallback", name, err)
	}

	set, err := settings.SetName("我的云盘")
	if err != nil || set != "我的云盘" {
		t.Errorf("SetName = (%q, %v)", set, err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"name":"\u6211\u7684\u4e91\u76d8"}`+"\n" {
		t.Errorf("persisted = %q, want ensure_ascii compact payload", raw)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}

	// A fresh instance (service restart) reads the same name.
	restarted, err := NewWebSettings(file, "其他名称")
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	name, err = restarted.Name()
	if err != nil || name != "我的云盘" {
		t.Errorf("after restart Name = (%q, %v), want 我 的云盘", name, err)
	}
}

func TestRootNameValidation(t *testing.T) {
	for _, value := range []string{"  我的云盘  ", "资料 / 2026", "云盘 <测试>"} {
		got, err := ValidateRootName(value)
		if err != nil {
			t.Errorf("ValidateRootName(%q) = %v", value, err)
			continue
		}
		if got != strings.TrimSpace(value) {
			t.Errorf("ValidateRootName(%q) = %q, want trimmed", value, got)
		}
	}

	for _, value := range []any{"", "   ", "bad\nname", "bad\x00name", 123, nil} {
		if _, err := ValidateRootName(value); err == nil {
			t.Errorf("ValidateRootName(%v) should be rejected", value)
		}
	}
	if _, err := ValidateRootName(strings.Repeat("a", MaxRootNameChars+1)); err == nil {
		t.Error("too many characters should be rejected")
	}
	// 256 runes x 4 UTF-8 bytes = exactly 1024, so the byte limit is
	// unreachable for valid UTF-8 within the character limit (the Python
	// check behaves the same). Multibyte names inside both limits pass.
	if _, err := ValidateRootName(strings.Repeat("云", 205)); err != nil {
		t.Errorf("multibyte name inside the limits = %v", err)
	}
	if _, err := ValidateRootName(strings.Repeat("\U0001F600", 256)); err != nil {
		t.Errorf("astral name at exactly 1024 bytes = %v", err)
	}
	// Control characters, including DEL, are rejected.
	for _, bad := range []string{"a\x01b", "a\x7fb"} {
		if _, err := ValidateRootName(bad); err == nil {
			t.Errorf("control character name %q should be rejected", bad)
		}
	}
}

func TestWebSettingsHotReload(t *testing.T) {
	dir := mkPrivateDir(t)
	file := filepath.Join(dir, "web-settings.json")
	settings, err := NewWebSettings(file, "初始云盘")
	if err != nil {
		t.Fatalf("NewWebSettings: %v", err)
	}

	if err := os.WriteFile(file, []byte(`{"name":"热加载"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, file)
	name, err := settings.Name()
	if err != nil || name != "热加载" {
		t.Errorf("Name after rewrite = (%q, %v), want 热加载", name, err)
	}

	// A broken file errors on access and keeps the applied name.
	if err := os.WriteFile(file, []byte(`{"name":`), 0o600); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, file)
	if _, err := settings.Name(); err == nil {
		t.Error("expected the broken file to raise on access")
	}
	settings.mu.Lock()
	if settings.name != "热加载" {
		t.Errorf("name after failed reload = %q, want the previous value", settings.name)
	}
	settings.mu.Unlock()
}

func TestWebSettingsFileQuirks(t *testing.T) {
	dir := mkPrivateDir(t)

	// A whitespace-only file falls back instead of failing.
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := NewWebSettings(empty, "初始云盘")
	if err != nil {
		t.Fatalf("empty file: %v", err)
	}
	name, err := settings.Name()
	if err != nil || name != "初始云盘" {
		t.Errorf("empty-file Name = (%q, %v), want fallback", name, err)
	}

	// A file without a name key fails closed, like validate_root_name(None).
	nameless := filepath.Join(dir, "nameless.json")
	if err := os.WriteFile(nameless, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWebSettings(nameless, "初始云盘"); err == nil ||
		err.Error() != "root name must be a string" {
		t.Errorf("nameless file = %v, want root name must be a string", err)
	}

	// An invalid name value fails closed.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"name":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWebSettings(bad, "初始云盘"); err == nil ||
		err.Error() != "root name must not be empty" {
		t.Errorf("empty name file = %v, want rejection", err)
	}
}

func TestWebSettingsFailClosedOnUnsafeFiles(t *testing.T) {
	dir := mkPrivateDir(t)
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte(`{"name":"safe"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWebSettings(link, "初始云盘"); err == nil ||
		err.Error() != "web settings file must be a regular file" {
		t.Errorf("symlink file = %v, want regular-file rejection", err)
	}

	loose := filepath.Join(dir, "loose.json")
	if err := os.WriteFile(loose, []byte(`{"name":"safe"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWebSettings(loose, "初始云盘"); err == nil ||
		err.Error() != "web settings file permissions are too broad" {
		t.Errorf("broad file = %v, want permission rejection", err)
	}

	openDir, err := os.MkdirTemp("", "settings-open")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(openDir)
	if err := os.Chmod(openDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// An existing but group-readable directory is "must be private";
	// the stat-failure wording needs an unreachable stat error as owner.
	_, err = NewWebSettings(filepath.Join(openDir, "web-settings.json"), "初始云盘")
	if err == nil || err.Error() != "web settings directory must be private" {
		t.Errorf("broad dir = %v, want web settings directory must be private", err)
	}
}

func TestWebSettingsErrorTypes(t *testing.T) {
	dir := mkPrivateDir(t)
	settings, err := NewWebSettings("", "初始云盘")
	if err != nil {
		t.Fatalf("memory-only settings: %v", err)
	}
	if _, err := settings.SetName(""); err == nil {
		t.Error("invalid SetName should fail")
	}
	name, err := settings.SetName("内存名")
	if err != nil || name != "内存名" {
		t.Errorf("SetName = (%q, %v)", name, err)
	}
	// Memory-only settings never touch the filesystem.
	if _, err := os.Stat(filepath.Join(dir, "web-settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("memory-only settings wrote a file: %v", err)
	}
	name, err = settings.Name()
	if err != nil || name != "内存名" {
		t.Errorf("memory-only Name = (%q, %v)", name, err)
	}
	var _ *SettingsError // value/type split stays importable
	var _ *SettingsFileError
}
