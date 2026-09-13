package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSpoolDirPrefersConfiguredThenResume(t *testing.T) {
	cases := []struct {
		spoolDir  string
		resumeDir string
		want      string
	}{
		{"", "", ""},
		{"/configured", "/resume", "/configured"},
		{"", "/resume", "/resume"},
	}
	for _, testCase := range cases {
		if got := resolveSpoolDir(testCase.spoolDir, testCase.resumeDir); got != testCase.want {
			t.Errorf("resolveSpoolDir(%q, %q) = %q, want %q", testCase.spoolDir, testCase.resumeDir, got, testCase.want)
		}
	}
}

func TestProbeSpoolDirSelfHealsMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	if warning := probeSpoolDir(dir, 8<<20); warning != "" {
		t.Fatalf("probe of a missing but creatable directory warned: %q", warning)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("probe did not create the spool directory: %v", err)
	}
}

func TestProbeSpoolDirWarnsWhenUnavailable(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	warning := probeSpoolDir(filepath.Join(blocker, "spool"), 8<<20)
	if warning == "" {
		t.Fatal("an unusable spool directory must produce a startup warning")
	}
	for _, fragment := range []string{"unavailable", "507", "8 MiB"} {
		if !strings.Contains(warning, fragment) {
			t.Errorf("warning %q does not mention %q", warning, fragment)
		}
	}
}

func TestProbeSpoolDirSkipsEmptyDirectory(t *testing.T) {
	if warning := probeSpoolDir("", 8<<20); warning != "" {
		t.Errorf("empty spool directory warned: %q", warning)
	}
}

// The assembly applies the resume-directory fallback to the budget: the
// container incident showed the OS-temp default is a real failure mode when
// the image carries no /tmp.
func TestNewFallsBackToResumeDirForSpool(t *testing.T) {
	cfg := withAuth(t, fixtureConfig(t))
	cfg.UploadSpoolDir = ""
	cfg.UploadResumeDir = t.TempDir()
	application, err := New(cfg, "test-1")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer application.Close()
	if got := application.Budget.SpoolDir(); got != cfg.UploadResumeDir {
		t.Errorf("budget spool dir = %q, want the resume directory fallback %q", got, cfg.UploadResumeDir)
	}
}
