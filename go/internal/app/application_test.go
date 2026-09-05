package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthzMatchesPythonContract(t *testing.T) {
	application := &Application{Version: "0.9.8"}
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body failed: %v", err)
	}

	want := `{"status":"ok","service":"wps-enterprise-adapter","version":"0.9.8","network_calls":"on-demand"}`
	if string(body) != want {
		t.Errorf("healthz body = %q, want %q", body, want)
	}
	if ct := response.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestUnknownRoutesFallBackTo404(t *testing.T) {
	application := &Application{Version: "0.9.8"}
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/definitely-not-here")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
}
