package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/httpserver"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

func TestDAVMutationsInvalidateSearchBeforeAndAfterWrite(t *testing.T) {
	cfg := withCredentials(t, fixtureConfig(t))
	started, release := make(chan struct{}), make(chan struct{})
	opener := refreshTestOpener(func(r *http.Request) (*http.Response, error) {
		body := `{"files":[{"id":1,"fname":"before.txt","ftype":"file"}],"next_offset":-1,"result":"ok"}`
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/files/folder") {
			close(started)
			<-release
			body = `{"id":2,"fname":"new","ftype":"folder","result":"ok"}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
	})
	application, err := New(cfg, "test", WithTransports(opener, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	handler, err := application.Handler()
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://adapter.invalid"+target, nil)
		request.Header.Set("Origin", "http://adapter.invalid")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	status := func() httpserver.SearchIndexStatus {
		t.Helper()
		recorder := call("GET", "/api/v1/search")
		var payload struct {
			Index httpserver.SearchIndexStatus `json:"index"`
		}
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &payload) != nil {
			t.Fatalf("search response: %d %s", recorder.Code, recorder.Body.String())
		}
		return payload.Index
	}
	refresh := func() httpserver.SearchIndexStatus {
		t.Helper()
		if recorder := call("POST", "/api/v1/search/refresh"); recorder.Code != http.StatusAccepted {
			t.Fatalf("refresh: %d %s", recorder.Code, recorder.Body.String())
		}
		deadline := time.After(3 * time.Second)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			current := status()
			if current.State == "ready" && current.Entries == 1 {
				return current
			}
			select {
			case <-ticker.C:
			case <-deadline:
				t.Fatalf("search did not finish: %+v", current)
			}
		}
	}
	initial := refresh()
	if recorder := call("HEAD", "/dav/"); recorder.Code != http.StatusOK {
		t.Fatalf("DAV read: %d %s", recorder.Code, recorder.Body.String())
	}
	if current := status(); current.Generation != initial.Generation || !current.Complete {
		t.Fatal("DAV read invalidated filename metadata")
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("MKCOL", "/dav/new") }()
	select {
	case <-started:
	case recorder := <-done:
		t.Fatalf("DAV mutation never reached fake WPS: %d %s", recorder.Code, recorder.Body.String())
	case <-time.After(3 * time.Second):
		t.Fatal("DAV mutation did not start")
	}
	// Always release the mutation even if an assertion fails.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if current := status(); current.State != "idle" || current.Entries != 0 {
		t.Fatalf("write began with old search snapshot still visible: %+v", current)
	}
	duringWrite := refresh()
	close(release)
	if recorder := <-done; recorder.Code != http.StatusCreated {
		t.Fatalf("DAV mutation: %d %s", recorder.Code, recorder.Body.String())
	}
	if current := status(); current.State != "idle" || current.Entries != 0 || current.Generation <= duringWrite.Generation {
		t.Fatalf("write completion retained an overlapping scan: %+v", current)
	}
}

func TestSearchIdentityFollowsCredentialAndWorkspaceFiles(t *testing.T) {
	cfg := withCredentials(t, fixtureConfig(t))
	state, err := workspace.NewWorkspaceState(cfg.WorkspaceFile, "", workspace.AutoValue)
	if err != nil {
		t.Fatal(err)
	}
	application := &Application{Config: cfg, Source: newCredentialSource(cfg), State: state}
	initial, err := application.searchIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if again, err := application.searchIdentity(); err != nil || again != initial {
		t.Fatal("unchanged local configuration changed the fingerprint")
	}
	if err := os.WriteFile(cfg.CookieFile, []byte("wps_cookie=another-invented-session"), 0o600); err != nil {
		t.Fatal(err)
	}
	credentialsChanged, err := application.searchIdentity()
	if err != nil || credentialsChanged == initial {
		t.Fatal("external credential replacement was not detected")
	}
	if err := os.WriteFile(cfg.WorkspaceFile, []byte(`{"group_id":"group-other","root_id":"root-other","spaces":[{"group_id":"group-other","name":"新空间","root_id":"0"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Match the hot state's normal stat throttle rather than reaching into
	// its private cache or requiring a service restart.
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		changed, err := application.searchIdentity()
		if err != nil {
			t.Fatal(err)
		}
		if changed != credentialsChanged {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("external workspace replacement was not detected")
		}
	}
	if err := os.WriteFile(cfg.CookieFile, []byte("invalid\ncredential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.searchIdentity(); err == nil {
		t.Fatal("unreadable credentials retained a valid search identity")
	}
}
