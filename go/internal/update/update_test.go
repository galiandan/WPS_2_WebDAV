package update

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeVersionAndNewerThan(t *testing.T) {
	cases := []struct {
		candidate string
		current   string
		want      bool
	}{
		{"v1.0.5", "1.0.4", true},
		{"1.1.0", "1.0.99", true},
		{"1.0.4", "v1.0.4", false},
		{"1.0.3", "1.0.4", false},
		{"latest", "1.0.4", false},
	}
	for _, tc := range cases {
		if got := newerThan(tc.candidate, tc.current); got != tc.want {
			t.Errorf("newerThan(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
	if got, ok := normalizeVersion("v1.2.3"); !ok || got != "1.2.3" {
		t.Fatalf("normalizeVersion = %q, %v", got, ok)
	}
	if _, ok := normalizeVersion("v1.2"); ok {
		t.Fatal("accepted a two-component version")
	}
}

func TestFetchLatestUsesArchitectureAsset(t *testing.T) {
	assetName := "wps-adapter-linux-" + runtime.GOARCH
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest" {
			t.Fatalf("path = %q, want /latest", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{
            "tag_name":"v1.0.5",
            "html_url":"https://github.com/galiandan/WPS_2_WebDAV/releases/tag/v1.0.5",
            "assets":[
              {"name":"checksums.txt","size":100},
              {"name":"%s","size":12345}
            ]
        }`, assetName)))
	}))
	defer server.Close()

	u := New("1.0.4")
	u.apiURL = server.URL + "/latest"
	u.client = server.Client()
	got, err := u.fetchLatest(context.Background())
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if got.Version != "1.0.5" || got.AssetName != assetName || got.AssetSize != 12345 {
		t.Fatalf("release = %+v", got)
	}
}

func TestFetchLatestRejectsNonHTTPSAPI(t *testing.T) {
	u := New("1.0.4")
	u.apiURL = "http://example.invalid/latest"
	if _, err := u.fetchLatest(context.Background()); err == nil {
		t.Fatal("accepted a non-HTTPS update API")
	}
}

func TestInstallReplacesTargetAfterVersionCheck(t *testing.T) {
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "wps-adapter")
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WPS_ADAPTER_UPDATE_BINARY", target)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.0.5/wps-adapter-linux-"+runtime.GOARCH {
			t.Fatalf("download path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("#!/bin/sh\nprintf '1.0.5 commit=test\\n'\n"))
	}))
	defer server.Close()

	u := New("1.0.4")
	u.assetBaseURL = server.URL
	u.client = server.Client()
	_, err := u.install(release{
		Version:   "1.0.5",
		Tag:       "v1.0.5",
		AssetName: "wps-adapter-linux-" + runtime.GOARCH,
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "1.0.5") {
		t.Fatalf("target was not replaced: %q", content)
	}
}
