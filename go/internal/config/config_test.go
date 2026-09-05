package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("ADAPTER_BIND", "")
	t.Setenv("ADAPTER_PORT", "")
	t.Setenv("ADAPTER_DAV_PREFIX", "")
	t.Setenv("ADAPTER_REST_PREFIX", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned %v", err)
	}
	if cfg.Bind != "127.0.0.1" {
		t.Errorf("Bind = %q, want 127.0.0.1", cfg.Bind)
	}
	if cfg.Port != 54321 {
		t.Errorf("Port = %d, want 54321", cfg.Port)
	}
	if cfg.DAVPrefix != "/dav" {
		t.Errorf("DAVPrefix = %q, want /dav", cfg.DAVPrefix)
	}
	if cfg.RESTPrefix != "/api/v1" {
		t.Errorf("RESTPrefix = %q, want /api/v1", cfg.RESTPrefix)
	}
}

func TestLoadPortOverrides(t *testing.T) {
	t.Setenv("ADAPTER_PORT", "8080")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
}

func TestLoadRejectsBrokenPortWithoutEchoingValue(t *testing.T) {
	for _, text := range []string{"abc", "0", "-1", "65536", "99999999"} {
		t.Setenv("ADAPTER_PORT", text)
		if _, err := Load(); err == nil {
			t.Errorf("Load() with ADAPTER_PORT=%q succeeded, want error", text)
		}
	}
}

func TestNormalisePrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/dav"},
		{"dav", "/dav"},
		{"/dav", "/dav"},
		{"/dav/", "/dav"},
		{"//", "/"},
	}
	for _, tc := range cases {
		if got := normalisePrefix(tc.in, "/dav"); got != tc.want {
			t.Errorf("normalisePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAuthEnabledCombinations(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"empty", Config{}, false},
		{"username only", Config{Username: "u"}, false},
		{"both literals", Config{Username: "u", Password: "p"}, true},
		{"both files", Config{UsernameFile: "u.txt", PasswordFile: "p.txt"}, true},
		{"one file", Config{UsernameFile: "u.txt"}, false},
	}
	for _, tc := range cases {
		if got := tc.cfg.AuthEnabled(); got != tc.want {
			t.Errorf("%s: AuthEnabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCheckPublicBind(t *testing.T) {
	local := Config{Bind: "127.0.0.1"}
	if err := local.CheckPublicBind(); err != nil {
		t.Errorf("local bind without auth should pass, got %v", err)
	}
	public := Config{Bind: "0.0.0.0"}
	if err := public.CheckPublicBind(); err == nil {
		t.Error("public bind without auth should be refused")
	}
	secured := Config{Bind: "0.0.0.0", Username: "u", Password: "p"}
	if err := secured.CheckPublicBind(); err != nil {
		t.Errorf("public bind with auth should pass, got %v", err)
	}
}
