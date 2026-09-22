package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/accounts"
)

func createScopedShare(t *testing.T, h *scopedAppHarness, user, path string) (string, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"path": path, "password": "2468"})
	response := h.request("POST", "/api/v1/shares", user, nil, string(body), nil)
	var result struct {
		URL   string `json:"url"`
		Share struct {
			ID string `json:"id"`
		} `json:"share"`
	}
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatalf("share creation %d %s", response.Code, response.Body.String())
	}
	parsed, err := url.Parse(result.URL)
	if err != nil || len(parsed.Fragment) != 64 {
		t.Fatalf("invalid share URL %q", result.URL)
	}
	return result.Share.ID, parsed.Fragment
}
func unlockScopedShare(t *testing.T, h *scopedAppHarness, id, token string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": token, "password": "2468"})
	response := h.request("POST", "/api/share/"+id+"/unlock", "", nil, string(body), nil)
	if response.Code != 200 {
		t.Fatalf("unlock %d %s", response.Code, response.Body.String())
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "wps_share_grant" {
			if !cookie.HttpOnly || cookie.Path != "/api/share/"+id || cookie.SameSite == http.SameSiteDefaultMode {
				t.Fatalf("unsafe grant cookie %+v", cookie)
			}
			return cookie
		}
	}
	t.Fatal("missing share grant")
	return nil
}
func TestPublicSharesUseNarrowGrantAndMemberScopedStorage(t *testing.T) {
	h := newScopedAppHarness(t)
	id, token := createScopedShare(t, h, "alice", "/")
	page := h.request("GET", "/share/"+id, "", nil, "", nil)
	if page.Code != 200 || page.Header().Get("WWW-Authenticate") != "" || page.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("guest page %d %v", page.Code, page.Header())
	}
	denied := h.request("GET", "/api/share/"+id+"/entries?path=/", "", nil, "", nil)
	if denied.Code < 400 {
		t.Fatal("public share exposed listing without grant")
	}
	grant := unlockScopedShare(t, h, id, token)
	listing := h.request("GET", "/api/share/"+id+"/entries?path=/", "", grant, "", nil)
	if listing.Code != 200 || !strings.Contains(listing.Body.String(), "alpha-only.txt") || strings.Contains(listing.Body.String(), "beta-only.txt") || strings.Contains(listing.Body.String(), "/alpha/") || strings.Contains(listing.Body.String(), token) {
		t.Fatalf("scoped guest list %d %s", listing.Code, listing.Body.String())
	}
	download := h.request("GET", "/api/share/"+id+"/download?path=/shared.txt", "", grant, "", nil)
	if download.Code != 200 || download.Body.String() != "content-201" {
		t.Fatalf("guest download %d %s", download.Code, download.Body.String())
	}
	for _, target := range []string{"/api/v1/entries?path=/", "/dav/shared.txt", "/api/share/" + id + "/entries?path=/../beta", "/api/share/" + id + "/entries?path=/%2e%2e/beta"} {
		result := h.request("GET", target, "", grant, "", nil)
		if result.Code < 400 {
			t.Fatalf("share grant escaped via %s: %d", target, result.Code)
		}
	}
	write := h.request("DELETE", "/api/share/"+id+"/entries?path=/shared.txt", "", grant, "", nil)
	if write.Code < 400 {
		t.Fatal("guest share accepted a mutation")
	}
	other := h.request("DELETE", "/api/v1/shares/"+id, "bob", nil, "", nil)
	if other.Code < 400 {
		t.Fatal("another member revoked share")
	}
	revoked := h.request("DELETE", "/api/v1/shares/"+id, "alice", nil, "", nil)
	if revoked.Code != 204 {
		t.Fatalf("revoke %d %s", revoked.Code, revoked.Body.String())
	}
	for _, action := range []string{"info", "entries?path=/", "download?path=/shared.txt"} {
		result := h.request("GET", "/api/share/"+id+"/"+action, "", grant, "", nil)
		if result.Code < 400 {
			t.Fatalf("revoked grant survived %s", action)
		}
	}
}
func TestPublicSharesCannotOutliveOwnerPolicyOrTargetIdentity(t *testing.T) {
	for _, change := range []string{"policy", "target"} {
		t.Run(change, func(t *testing.T) {
			h := newScopedAppHarness(t)
			id, token := createScopedShare(t, h, "alice", "/shared.txt")
			grant := unlockScopedShare(t, h, id, token)
			if change == "policy" {
				enabled := false
				if _, err := h.app.Accounts.Update(h.alice.ID, accounts.UpdateUser{Enabled: &enabled}); err != nil {
					t.Fatal(err)
				}
			} else {
				h.remote.mu.Lock()
				h.remote.children["101"][0]["id"] = "999"
				h.remote.mu.Unlock()
			}
			response := h.request("GET", "/api/share/"+id+"/download?path=/", "", grant, "", nil)
			if response.Code < 400 {
				t.Fatalf("share survived %s change: %d %s", change, response.Code, response.Body.String())
			}
		})
	}
}
