package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
)

func TestWebAuthUsesInstallationCredentialsAndKeepsDAVBasicChallenge(t *testing.T) {
	store := auth.NewStore(func() (string, string) { return "dav", "dav-secret" })
	dispatcher := newReadDispatcher(t, &fakeReadStorage{}, nil)
	dispatcher.SetWebAuth(store)
	router := readRouter(t, dispatcher)
	chain, err := NewChain(ChainConfig{
		Router:  router,
		Health:  func(w http.ResponseWriter, r *http.Request) {},
		Auth:    BasicAuthConfig{Username: "dav", Password: "dav-secret"},
		WebAuth: &WebAuthConfig{Store: store, RESTPrefix: "/api/v1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	response := serveWebAuthRequest(t, chain, http.MethodGet, "/", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("public web page status = %d", response.Code)
	}
	response = serveWebAuthRequest(t, chain, http.MethodGet, "/api/v1/auth/me", "", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"authenticated":false`) {
		t.Fatalf("anonymous me = %d %s", response.Code, response.Body.String())
	}
	response = serveWebAuthRequest(t, chain, http.MethodPost, "/api/v1/auth/register", `{"username":"other","password":"long enough password"}`, "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("register endpoint = %d %s", response.Code, response.Body.String())
	}
	response = serveWebAuthRequest(t, chain, http.MethodPost, "/api/v1/auth/login", `{"username":"dav","password":"wrong"}`, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong login = %d %s", response.Code, response.Body.String())
	}

	response = serveWebAuthRequest(t, chain, http.MethodPost, "/api/v1/auth/login", `{"username":"dav","password":"dav-secret"}`, "")
	if response.Code != http.StatusOK {
		t.Fatalf("login = %d %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != auth.SessionCookieName || !cookies[0].HttpOnly {
		t.Fatalf("session cookie = %+v", cookies)
	}
	token := cookies[0].Value

	response = serveWebAuthRequest(t, chain, http.MethodGet, "/api/v1/settings", "", token)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated settings = %d %s", response.Code, response.Body.String())
	}
	response = serveWebAuthRequest(t, chain, http.MethodGet, "/api/v1/settings", "", "")
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("anonymous browser API = %d challenge=%q body=%s", response.Code, response.Header().Get("WWW-Authenticate"), response.Body.String())
	}
	response = serveWebAuthRequest(t, chain, http.MethodGet, "/dav/", "", token)
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("DAV cookie must not replace Basic Auth = %d challenge=%q", response.Code, response.Header().Get("WWW-Authenticate"))
	}
	response = serveWebAuthRequest(t, chain, http.MethodGet, "/dav/", "", "", basicCredentials("dav", "dav-secret"))
	if response.Code != http.StatusOK {
		t.Fatalf("DAV basic auth = %d", response.Code)
	}

	response = serveWebAuthRequest(t, chain, http.MethodPost, "/api/v1/auth/logout", "", token)
	if response.Code != http.StatusOK {
		t.Fatalf("logout = %d %s", response.Code, response.Body.String())
	}
	response = serveWebAuthRequest(t, chain, http.MethodGet, "/api/v1/settings", "", token)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("logged out settings = %d", response.Code)
	}
}

func serveWebAuthRequest(t *testing.T, handler http.Handler, method, target, body, token string, authorization ...string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	if token != "" {
		request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	}
	if len(authorization) > 0 {
		request.Header.Set("Authorization", authorization[0])
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestAuthMeDoesNotExposeCredentialFields(t *testing.T) {
	store := auth.NewStore(func() (string, string) { return "alice", "long enough password" })
	token, _, err := store.Login("alice", "long enough password")
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := newReadDispatcher(t, &fakeReadStorage{}, nil)
	dispatcher.SetWebAuth(store)
	router := readRouter(t, dispatcher)
	chain, err := NewChain(ChainConfig{Router: router, Health: func(w http.ResponseWriter, r *http.Request) {}, WebAuth: &WebAuthConfig{Store: store, RESTPrefix: "/api/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	response := serveWebAuthRequest(t, chain, http.MethodGet, "/api/v1/auth/me", "", token)
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"password", "password_hash", "id", "created_at"} {
		if _, found := payload[field]; found {
			t.Fatalf("credential field %q leaked from auth/me", field)
		}
	}
}
