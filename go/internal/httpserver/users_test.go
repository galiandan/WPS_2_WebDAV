package httpserver

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/accounts"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func usersFixture(t *testing.T) (http.Handler, *RESTDispatcher, *accounts.Store, *auth.AccountStores) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	credentials := func() (string, string) { return "admin", "admin-password" }
	registry, err := accounts.New(filepath.Join(dir, "users.json"), credentials)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := auth.NewPersistentStore(credentials, filepath.Join(dir, "auth-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub, err := auth.NewAccountStores(admin, registry)
	if err != nil {
		t.Fatal(err)
	}
	d := newReadDispatcher(t, &fakeReadStorage{}, nil)
	d.SetWebAuth(admin)
	d.SetAccounts(hub)
	d.SetUsers(registry, func(path string) (model.RemoteEntry, error) {
		if path != "/Space/private" {
			return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "not found")
		}
		return model.RemoteEntry{ID: "bound-root", Name: "private", Kind: model.KindFolder}, nil
	}, func(string) (string, error) { return strings.Repeat("a", 64), nil })
	chain, err := NewChain(ChainConfig{Router: readRouter(t, d), Health: func(http.ResponseWriter, *http.Request) {}, Auth: BasicAuthConfig{Provider: registry}, WebAuth: &WebAuthConfig{Store: admin, Accounts: hub, RESTPrefix: "/api/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	return chain, d, registry, hub
}

func TestUsersAdminCRUDRootBindingAndMemberDenial(t *testing.T) {
	chain, _, registry, _ := usersFixture(t)
	admin := basicCredentials("admin", "admin-password")
	created := serveWebAuthRequest(t, chain, "POST", "/api/v1/users", `{"username":"alice","password":"member-password","root_path":"/Space/private","permissions":{"read":true,"upload":true,"delete":false}}`, "", admin)
	if created.Code != 201 {
		t.Fatalf("code=%d body=%s", created.Code, created.Body.String())
	}
	var result struct {
		User accounts.User `json:"user"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.User.RootID != "bound-root" || result.User.RootPath != "/Space/private" {
		t.Fatal(result.User)
	}
	login := serveWebAuthRequest(t, chain, "POST", "/api/v1/auth/login", `{"username":"alice","password":"member-password"}`, "")
	if login.Code != 200 || strings.Contains(login.Body.String(), "/Space/private") || strings.Contains(login.Body.String(), "bound-root") {
		t.Fatalf("member login=%d %s", login.Code, login.Body.String())
	}
	token := login.Result().Cookies()[0].Value
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		target := "/api/v1/users"
		if method == "PATCH" || method == "DELETE" {
			target += "/" + result.User.ID
		}
		for _, cookie := range []bool{false, true} {
			var responseCode int
			if cookie {
				responseCode = serveWebAuthRequest(t, chain, method, target, `{}`, token).Code
			} else {
				responseCode = serveWebAuthRequest(t, chain, method, target, `{}`, "", basicCredentials("alice", "member-password")).Code
			}
			if responseCode != 403 && !(method == "GET" && responseCode == 400) {
				t.Fatalf("member %s cookie=%t status=%d", method, cookie, responseCode)
			}
		}
	}
	changed := serveWebAuthRequest(t, chain, "PATCH", "/api/v1/users/"+result.User.ID, `{"enabled":false}`, "", admin)
	if changed.Code != 200 {
		t.Fatalf("update=%d %s", changed.Code, changed.Body.String())
	}
	if _, ok := registry.LookupID(result.User.ID); ok {
		t.Fatal("disabled account still active")
	}
	if response := serveWebAuthRequest(t, chain, "GET", "/api/v1/settings", "", token); response.Code != 401 {
		t.Fatal("disabled browser session survived", response.Code)
	}
	if response := serveWebAuthRequest(t, chain, "GET", "/api/v1/settings", "", "", basicCredentials("alice", "member-password")); response.Code != 401 {
		t.Fatal("disabled Basic cache survived", response.Code)
	}
	deleted := serveWebAuthRequest(t, chain, "DELETE", "/api/v1/users/"+result.User.ID, "", "", admin)
	if deleted.Code != 204 {
		t.Fatalf("delete=%d %s", deleted.Code, deleted.Body.String())
	}
}

func TestMemberSecurityEndpointsCannotReadAdminFactors(t *testing.T) {
	chain, _, registry, hub := usersFixture(t)
	const adminSecret = "JBSWY3DPEHPK3PXP"
	secret, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(adminSecret)
	mac := hmac.New(sha1.New, secret)
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(time.Now().Unix()/30))
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 15
	number := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	if _, err := hub.AdminStore().EnableTOTP(adminSecret, fmt.Sprintf("%06d", number%1000000)); err != nil {
		t.Fatal(err)
	}
	member, err := registry.Create(accounts.CreateUser{Username: "alice", Password: "member-password", RootPath: "/Space/private", RootID: "bound-root", RootBinding: strings.Repeat("a", 64), Permissions: auth.Permissions{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := hub.ForPrincipal(member.Principal)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.Login("alice", "member-password")
	if err != nil {
		t.Fatal(err)
	}
	response := serveWebAuthRequest(t, chain, "GET", "/api/v1/auth/security", "", token)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"totp_enabled":false`) {
		t.Fatalf("security=%d %s", response.Code, response.Body.String())
	}
	setup := serveWebAuthRequest(t, chain, "POST", "/api/v1/auth/totp/setup", "", token)
	if setup.Code != 200 || !strings.Contains(setup.Body.String(), "alice") {
		t.Fatalf("setup=%d %s", setup.Code, setup.Body.String())
	}
	if !hub.AdminStore().TwoFactorEnabled() || store.TwoFactorEnabled() {
		t.Fatal("member factor operation changed the admin configuration")
	}
	options := serveWebAuthRequest(t, chain, "POST", "/api/v1/auth/passkey/register/options", "", token)
	if options.Code != 200 || !strings.Contains(options.Body.String(), `"name":"alice"`) || strings.Contains(options.Body.String(), `"name":"admin"`) {
		t.Fatalf("options=%d %s", options.Code, options.Body.String())
	}
}

func TestBasicAndBrowserRequestsCarryFreshPrincipal(t *testing.T) {
	_, _, registry, hub := usersFixture(t)
	member, err := registry.Create(accounts.CreateUser{Username: "alice", Password: "member-password", RootPath: "/Space/private", RootID: "bound-root", RootBinding: strings.Repeat("a", 64), Permissions: auth.Permissions{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	var seen auth.Principal
	chain, err := NewChain(ChainConfig{Router: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = auth.PrincipalFromContext(r.Context())
		w.WriteHeader(200)
	}), Health: func(http.ResponseWriter, *http.Request) {}, Auth: BasicAuthConfig{Provider: registry}, WebAuth: &WebAuthConfig{Store: hub.AdminStore(), Accounts: hub, RESTPrefix: "/api/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	response := serveWebAuthRequest(t, chain, "GET", "/dav/file", "", "", basicCredentials("alice", "member-password"))
	if response.Code != 200 || seen.ID != member.ID || seen.RootID != "bound-root" {
		t.Fatalf("principal=%+v code=%d", seen, response.Code)
	}
	store, _ := hub.ForPrincipal(member.Principal)
	token, _, err := store.Login("alice", "member-password")
	if err != nil {
		t.Fatal(err)
	}
	seen = auth.Principal{}
	response = serveWebAuthRequest(t, chain, "GET", "/api/v1/entries", "", token)
	if response.Code != 200 || seen.ID != member.ID {
		t.Fatalf("cookie principal=%+v code=%d", seen, response.Code)
	}
}

func TestUsersRejectNamespaceChangeDuringRootResolution(t *testing.T) {
	chain, d, registry, _ := usersFixture(t)
	calls := 0
	d.userRootBindingResolver = func(string) (string, error) {
		calls++
		if calls == 1 {
			return strings.Repeat("a", 64), nil
		}
		return strings.Repeat("b", 64), nil
	}
	response := serveWebAuthRequest(t, chain, "POST", "/api/v1/users", `{"username":"alice","password":"member-password","root_path":"/Space/private","permissions":{"read":true,"upload":false,"delete":false}}`, "", basicCredentials("admin", "admin-password"))
	if response.Code != 409 || len(registry.List()) != 1 {
		t.Fatalf("namespace changed but account was granted: %d %s", response.Code, response.Body.String())
	}
}

func TestSessionImportSlashAliasesRemainBasicOnly(t *testing.T) {
	chain, _, _, hub := usersFixture(t)
	token, _, err := hub.AdminStore().Login("admin", "admin-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/api/v1/session/import", "/api/v1/session/import/", "/api/v1//session/import/", "/api/v1///session/import//"} {
		response := serveWebAuthRequest(t, chain, "POST", target, `{}`, token)
		if response.Code != 401 {
			t.Fatalf("cookie accessed %s: %d %s", target, response.Code, response.Body.String())
		}
	}
}

type actionOnRead struct {
	io.Reader
	once   sync.Once
	action func()
}

func (r *actionOnRead) Read(body []byte) (int, error) {
	r.once.Do(r.action)
	return r.Reader.Read(body)
}
func (r *actionOnRead) Close() error { return nil }

func TestFactorMutationRevalidatesSessionAfterReadingBody(t *testing.T) {
	chain, _, registry, hub := usersFixture(t)
	member, err := registry.Create(accounts.CreateUser{Username: "alice", Password: "member-password", RootPath: "/Space/private", RootID: "bound-root", RootBinding: strings.Repeat("a", 64), Permissions: auth.Permissions{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	store, _ := hub.ForPrincipal(member.Principal)
	token, _, err := store.Login("alice", "member-password")
	if err != nil {
		t.Fatal(err)
	}
	request := writeRequest("POST", "/api/v1/auth/totp/enable", nil, `{"secret":"JBSWY3DPEHPK3PXP","code":"000000"}`)
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	request.Body = &actionOnRead{Reader: request.Body, action: func() {
		disabled := false
		if _, err := registry.Update(member.ID, accounts.UpdateUser{Enabled: &disabled}); err != nil {
			t.Error(err)
		}
	}}
	response := httptest.NewRecorder()
	chain.ServeHTTP(response, request)
	if response.Code != 401 || store.TwoFactorEnabled() {
		t.Fatalf("revoked factor mutation=%d %s", response.Code, response.Body.String())
	}
}
