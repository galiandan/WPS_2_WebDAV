package wps

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClientRequiresGroupIDOrWorkspace(t *testing.T) {
	if _, err := NewClient(DefaultConfig("")); err == nil || err.Error() != "group_id or workspace state is required" {
		t.Fatalf("error = %v, want group_id or workspace state is required", err)
	}
	if _, err := NewClient(DefaultConfig("group-1")); err != nil {
		t.Fatalf("NewClient with group id failed: %v", err)
	}
}

func TestNewClientValidatesMaxJSONResponseBytes(t *testing.T) {
	config := DefaultConfig("group-1")
	config.MaxJSONResponseBytes = 0
	if _, err := NewClient(config); err == nil || err.Error() != "max_json_response_bytes must be positive" {
		t.Fatalf("error = %v, want max_json_response_bytes must be positive", err)
	}
}

func TestNewClientValidatesBaseURL(t *testing.T) {
	config := DefaultConfig("group-1")
	config.BaseURL = "http://365.kdocs.cn"
	if _, err := NewClient(config); err == nil ||
		err.Error() != "base_url must be an HTTPS WPS host without a path or credentials" {
		t.Fatalf("error = %v, want the fixed base_url message", err)
	}
}

func TestNewClientAcceptsAndRejectsBaseURLShapes(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{"plain host", "https://365.kdocs.cn", false},
		{"root path", "https://365.kdocs.cn/", false},
		{"case insensitive host", "https://KDOCS.CN/", false},
		{"trailing dot host", "https://kdocs.cn./", false},
		{"empty userinfo ignored like Python truthiness", "https://@365.kdocs.cn", false},
		{"wrong scheme", "http://365.kdocs.cn", true},
		{"missing host", "https://", true},
		{"outside kdocs", "https://attacker.example/", true},
		{"userinfo", "https://user:pass@365.kdocs.cn/", true},
		{"password only userinfo", "https://:pass@365.kdocs.cn/", true},
		{"query", "https://365.kdocs.cn/?x=1", true},
		{"fragment", "https://365.kdocs.cn/#f", true},
		{"path", "https://365.kdocs.cn/api", true},
		{"raw encoded path", "https://365.kdocs.cn/%2F", true},
		{"dot segments kept raw", "https://365.kdocs.cn/a/../", true},
		{"invalid escape", "https://365.kdocs.cn/%gg", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultConfig("group-1")
			config.BaseURL = test.baseURL
			_, err := NewClient(config)
			if (err != nil) != test.wantErr {
				t.Fatalf("NewClient(%q) error = %v, wantErr %v", test.baseURL, err, test.wantErr)
			}
			if err != nil && err.Error() != "base_url must be an HTTPS WPS host without a path or credentials" {
				t.Fatalf("error = %v, want the fixed base_url message", err)
			}
		})
	}
}

func TestNewClientValidatesObjectSuffix(t *testing.T) {
	config := DefaultConfig("group-1")
	config.ObjectStorageHostSuffix = "attacker.example"
	if _, err := NewClient(config); err == nil ||
		err.Error() != "object_storage_host_suffix must be within kdocs.cn" {
		t.Fatalf("error = %v, want the within kdocs.cn message", err)
	}
	config.ObjectStorageHostSuffix = ""
	if _, err := NewClient(config); err == nil {
		t.Fatal("empty suffix must be rejected")
	}
	for _, suffix := range []string{".ag.kdocs.cn", "kdocs.cn", "AG.KDOCS.CN", ".kdocs.cn."} {
		config.ObjectStorageHostSuffix = suffix
		if _, err := NewClient(config); err != nil {
			t.Fatalf("suffix %q rejected: %v", suffix, err)
		}
	}
}

func TestNewClientValidatesStatusTimings(t *testing.T) {
	config := DefaultConfig("group-1")
	config.StatusProbeTTL = -1
	config.StatusFailureBackoff = -1
	if _, err := NewClient(config); err == nil || err.Error() != "status_probe_ttl must not be negative" {
		t.Fatalf("error = %v, want the probe TTL message first", err)
	}
	config.StatusProbeTTL = 30
	if _, err := NewClient(config); err == nil || err.Error() != "status_failure_backoff must not be negative" {
		t.Fatalf("error = %v, want the backoff message", err)
	}
}

func TestDefaultConfigMatchesPythonDefaults(t *testing.T) {
	config := DefaultConfig("group-1")
	if config.GroupID != "group-1" || config.BaseURL != "https://365.kdocs.cn" ||
		config.AccountBaseURL != "" || config.AutoRefresh != true || config.Referer != "" ||
		config.Origin != "" || config.CID != "" || config.Timeout != 30 ||
		config.StatusProbeTTL != 30 || config.StatusFailureBackoff != 5 ||
		config.UploadSpoolMemory != 8<<20 || config.StreamChunkSize != 1<<20 ||
		config.MultipartThreshold != 50<<20 || config.MultipartPartSize != 10<<20 ||
		config.EnableRange != true || config.UploadSpoolDir != "" || config.UploadResumeDir != "" ||
		config.UploadMinFreeBytes != 512<<20 || config.MaxUploadBytes != 1<<30 ||
		config.UploadRetries != 2 || config.UploadRetryDelay != 0.5 ||
		config.ObjectStorageHostSuffix != ".ag.kdocs.cn" || config.MaxJSONResponseBytes != 8<<20 {
		t.Fatalf("defaults diverge from the Python dataclass: %+v", config)
	}
}

func TestResponseLimitsMirrorClientPy(t *testing.T) {
	if MaxJSONResponseBytes != 8<<20 || MaxObjectResponseBytes != 1<<20 ||
		MaxXMLResponseBytes != 4<<20 || DefaultMaxUploadBytes != 1024<<20 ||
		MaxMultipartPartBuffer != 64<<20 || MaxRemoteNameBytes != 4096 ||
		MaxRemoteEtagBytes != 4096 {
		t.Fatalf("response limits diverge from client.py constants")
	}
}

func TestControlTransportRefusesRedirects(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Location", "/next")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	client, err := NewClient(DefaultConfig("group-1"))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	response, err := client.opener.Do(request)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 surfaced without following", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || len(body) != 0 {
		t.Fatalf("3xx body = %q err = %v", body, err)
	}
	if hits != 1 {
		t.Fatalf("server hits = %d, want 1 (no redirect follow)", hits)
	}
}

func TestControlTransportHasNoCookieJarAndDistinctTransports(t *testing.T) {
	client, err := NewClient(DefaultConfig("group-1"))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	httpClient, ok := client.opener.(*http.Client)
	if !ok {
		t.Fatalf("opener is %T, want *http.Client", client.opener)
	}
	if httpClient.Jar != nil {
		t.Fatal("control client must not carry a cookie jar")
	}
	if client.signed.transport == nil {
		t.Fatal("signed transport missing")
	}
	if _, same := client.signed.transport.(*http.Transport); !same {
		t.Fatalf("signed transport is %T", client.signed.transport)
	}
	if interface{}(httpClient.Transport) == interface{}(client.signed.transport) {
		t.Fatal("control and signed clients must not share a transport")
	}
}

func TestControlTransportTimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := DefaultConfig("group-1")
	config.Timeout = 0.05
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		response, err := client.opener.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Client.Timeout") {
			t.Fatalf("error = %v, want a client timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Do did not return within the test deadline")
	}
}

func TestControlTransportVerifiesTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(DefaultConfig("group-1"))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	_, err = client.opener.Do(request)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("error = %v, want a certificate verification failure", err)
	}
}

func TestControlClientTimeoutsBoundConnectionPhases(t *testing.T) {
	client, err := NewClient(DefaultConfig("group-1"))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	httpClient := client.opener.(*http.Client)
	transport := httpClient.Transport.(*http.Transport)
	if httpClient.Timeout != 30*time.Second {
		t.Fatalf("client timeout = %v, want 30s", httpClient.Timeout)
	}
	if transport.TLSHandshakeTimeout != 30*time.Second || transport.ResponseHeaderTimeout != 30*time.Second {
		t.Fatalf("phase timeouts = %v/%v, want 30s", transport.TLSHandshakeTimeout, transport.ResponseHeaderTimeout)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("control transport must pin TLS 1.2 as the minimum version")
	}
}
