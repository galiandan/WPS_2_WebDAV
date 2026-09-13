package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// probeSpoolUsable must be faked out for resolution tests: the real probe
// touches the filesystem, and candidates like "/configured" do not exist.
func withUsableProbe(t *testing.T, usable map[string]error) {
	t.Helper()
	original := probeSpoolUsable
	probeSpoolUsable = func(dir string) error {
		if err, ok := usable[dir]; ok {
			return err
		}
		return errors.New("unavailable in test")
	}
	t.Cleanup(func() { probeSpoolUsable = original })
}

func TestResolveSpoolDirPrefersFirstUsableCandidate(t *testing.T) {
	cases := []struct {
		spoolDir  string
		resumeDir string
		usable    map[string]error
		wantDir   string
		wantFrom  string
	}{
		{
			spoolDir: "/configured", resumeDir: "/resume",
			usable:   map[string]error{"/configured": nil},
			wantDir:  "/configured",
			wantFrom: "configured",
		},
		{
			spoolDir: "/configured", resumeDir: "/resume",
			usable:   map[string]error{"/configured": errors.New("ro mount"), "/resume": nil},
			wantDir:  "/resume",
			wantFrom: "resume",
		},
		{
			spoolDir: "", resumeDir: "",
			usable:   map[string]error{os.TempDir(): nil},
			wantDir:  os.TempDir(),
			wantFrom: "temp",
		},
		{
			spoolDir: "", resumeDir: "",
			usable:   map[string]error{},
			wantDir:  "",
			wantFrom: "",
		},
	}
	for _, testCase := range cases {
		withUsableProbe(t, testCase.usable)
		gotDir, gotFrom := resolveSpoolDir(testCase.spoolDir, testCase.resumeDir)
		if gotDir != testCase.wantDir || gotFrom != testCase.wantFrom {
			t.Errorf("resolveSpoolDir(%q, %q) = (%q, %q), want (%q, %q)",
				testCase.spoolDir, testCase.resumeDir, gotDir, gotFrom, testCase.wantDir, testCase.wantFrom)
		}
	}
}

// The executable-side candidate rescues deployments whose environment
// predates the spool env vars: containers created before WPS_UPLOAD_SPOOL_DIR
// existed carry neither variable, and images without /tmp used to fail every
// large upload with 507.
func TestResolveSpoolDirFallsBackBesideExecutable(t *testing.T) {
	beside := spoolDirBesideExecutable()
	if beside == "" {
		t.Skip("os.Executable unavailable")
	}
	if filepath.Base(beside) != "spool" {
		t.Errorf("executable candidate %q does not end in spool", beside)
	}
	withUsableProbe(t, map[string]error{
		os.TempDir(): errors.New("no /tmp in container"),
		beside:       nil,
	})
	gotDir, gotFrom := resolveSpoolDir("", "")
	if gotFrom != "executable" || gotDir != beside {
		t.Errorf("resolveSpoolDir(\"\", \"\") = (%q, %q), want the executable fallback (%q, executable)", gotDir, gotFrom, beside)
	}
}

func TestProbeSpoolUsableRejectsUnwritableDirectory(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := probeSpoolUsable(filepath.Join(blocker, "spool")); err == nil {
		t.Fatal("MkdirAll under a file must fail")
	}
	if err := probeSpoolUsable(t.TempDir()); err != nil {
		t.Fatalf("a writable directory must pass: %v", err)
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

// The assembly applies the fallback chain to the budget: the container
// incident showed the OS-temp default is a real failure mode when the image
// carries no /tmp and the environment predates the spool env vars.
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
