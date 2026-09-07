package wps

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

const isloginBody = `{"islogin":true,"is_company_account":true}`
const listOKBody = `{"files":[],"next_offset":-1,"result":"ok"}`

// gatedOpener blocks the first request until released, then serves a
// canned sequence, mirroring the Python SlowOpener singleflight fixture.
type gatedOpener struct {
	mu       sync.Mutex
	requests []*http.Request
	gate     chan struct{}
	release  chan struct{}
}

func (o *gatedOpener) Do(request *http.Request) (*http.Response, error) {
	o.mu.Lock()
	o.requests = append(o.requests, request)
	count := len(o.requests)
	o.mu.Unlock()
	if count == 1 {
		close(o.gate)
		<-o.release
	}
	body := listOKBody
	if count == 1 {
		body = isloginBody
	}
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        http.Header{},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}, nil
}

func TestStatusPreflightChecksLoginAndWorkspaceOnce(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"companyid":691045587,"is_company_account":true}`)},
		{status: 200, body: []byte(listOKBody)},
	}}
	client.opener = opener

	first, err := client.CheckStatus("0")
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	second, err := client.CheckStatus("0")
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}

	if first.Status != "connected" || first.Wps != "connected" ||
		first.Workspace != "ready" || first.AccountType != "business" || first.RetryAfter != 0 {
		t.Fatalf("first status = %+v", first)
	}
	if second != first {
		t.Fatalf("second status = %+v, want cached %+v", second, first)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(opener.requests))
	}
	account := opener.requests[0]
	if account.URL.Host != "account.kdocs.cn" || account.URL.Path != "/api/v3/islogin" {
		t.Fatalf("account URL = %v", account.URL)
	}
	if account.Header.Get("Cookie") != "Cookie-secret" {
		t.Fatalf("account Cookie = %q", account.Header.Get("Cookie"))
	}
	wantQuery := "parentid=0&offset=0&count=1&orderby=mtime&order=desc"
	if opener.requests[1].URL.RawQuery != wantQuery {
		t.Fatalf("list query = %q, want %q", opener.requests[1].URL.RawQuery, wantQuery)
	}
}

func TestStatusWithoutCredentialsDoesNotCallWPS(t *testing.T) {
	config := DefaultConfig("group-1")
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{}
	client.opener = opener

	result, err := client.CheckStatus("0")
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	if result.Status != "not_configured" || result.Wps != "not_configured" ||
		result.Workspace != "not_configured" {
		t.Fatalf("status = %+v", result)
	}
	if len(opener.requests) != 0 {
		t.Fatalf("requests = %d, want 0", len(opener.requests))
	}
}

func TestStatusTreatsMissingCredentialFilesAsNotConfigured(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	config := DefaultConfig("group-1")
	config.CredentialSource = &credentials.FileCredentialSource{
		CookiePath:    filepath.Join(directory, "cookie"),
		CSRFTokenPath: filepath.Join(directory, "csrf"),
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{}
	client.opener = opener

	result, err := client.CheckStatus("0")
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	if result.Status != "not_configured" {
		t.Fatalf("status = %+v", result)
	}
	if len(opener.requests) != 0 {
		t.Fatalf("requests = %d, want 0", len(opener.requests))
	}
}

func TestStatusMarksAnExpiredSessionWithoutRefreshing(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{{status: 401}}}
	client.opener = opener

	result, err := client.CheckStatus("0")
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	if result.Status != "session_expired" {
		t.Fatalf("status = %+v", result)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}
	if opener.requests[0].URL.Path != "/api/v3/islogin" {
		t.Fatalf("request path = %v", opener.requests[0].URL)
	}
}

func TestStatusDistinguishesWorkspacePermissionFailure(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(isloginBody)},
		{status: 403},
	}}
	client.opener = opener

	result, err := client.CheckStatus("private-root")
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	if result.Status != "permission_denied" || result.Wps != "connected" ||
		result.Workspace != "permission_denied" {
		t.Fatalf("status = %+v", result)
	}
}

func TestStatusMarksMalformedLoginResponse(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`[]`)},
	}}
	client.opener = opener

	result, err := client.CheckStatus("0")
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	if result.Status != "invalid_response" {
		t.Fatalf("status = %+v", result)
	}
}

func TestStatusFailureBackoffReusesTheLastFailure(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	config.StatusFailureBackoff = 30
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{{status: 401}}}
	client.opener = opener

	first, err := client.CheckStatus("0")
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	second, err := client.CheckStatus("0")
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	if first.Status != "session_expired" || second.Status != "session_expired" {
		t.Fatalf("statuses = %+v / %+v", first, second)
	}
	if second.RetryAfter < 1 {
		t.Fatalf("retry_after = %d, want at least 1", second.RetryAfter)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}
}

func TestStatusSingleflightMergesConcurrentChecks(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &gatedOpener{
		gate:    make(chan struct{}),
		release: make(chan struct{}),
	}
	client.opener = opener

	results := make([]model.WpsStatus, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for index := 0; index < 2; index++ {
		go func(slot int) {
			defer ready.Done()
			result, err := client.CheckStatus("0")
			if err != nil {
				t.Errorf("CheckStatus failed: %v", err)
				return
			}
			results[slot] = result
		}(index)
	}
	<-opener.gate
	// Give the second goroutine a moment to enter the wait, then release.
	time.Sleep(50 * time.Millisecond)
	close(opener.release)
	done := make(chan struct{})
	go func() {
		ready.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent checks did not finish")
	}
	for _, result := range results {
		if result.Status != "connected" {
			t.Fatalf("status = %+v, want connected", result)
		}
	}
	opener.mu.Lock()
	defer opener.mu.Unlock()
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (islogin + list, single flight)", len(opener.requests))
	}
}

func TestStatusRootList401DoesNotRefresh(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(isloginBody)},
		{status: 401},
	}}
	client.opener = opener

	result, err := client.CheckStatus("0")
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	if result.Status != "session_expired" {
		t.Fatalf("status = %+v, want session_expired without refresh", result)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want 2 with no grant request", len(opener.requests))
	}
	if opener.requests[1].URL.Path != "/3rd/drive/api/v5/groups/group-1/files" {
		t.Fatalf("list path = %v", opener.requests[1].URL)
	}
}

func TestStatusCacheMarkerInvalidatesOnRootChange(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(isloginBody)},
		{status: 200, body: []byte(listOKBody)},
		{status: 200, body: []byte(isloginBody)},
		{status: 200, body: []byte(listOKBody)},
	}}
	client.opener = opener

	if _, err := client.CheckStatus("0"); err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	if _, err := client.CheckStatus("private-root"); err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	if len(opener.requests) != 4 {
		t.Fatalf("requests = %d, want 4 after the marker changed", len(opener.requests))
	}
}

func TestStatusUpstreamUnavailableMapping(t *testing.T) {
	tests := []struct {
		name    string
		script  []scriptedResponse
		failure []error
	}{
		{"transport failure", nil, []error{errFakeTransport}},
		{"http 500", []scriptedResponse{{status: 500}}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultConfig("group-1")
			config.CredentialSource = staticSource()
			client, err := NewClient(config)
			if err != nil {
				t.Fatalf("NewClient failed: %v", err)
			}
			opener := &fakeControlOpener{script: test.script, failures: test.failure}
			client.opener = opener

			result, err := client.CheckStatus("0")
			if err != nil {
				t.Fatalf("CheckStatus failed: %v", err)
			}
			if result.Status != "upstream_unavailable" || result.Wps != "unknown" ||
				result.Workspace != "unknown" {
				t.Fatalf("status = %+v", result)
			}
			if result.RetryAfter != 0 {
				t.Fatalf("retry_after = %d, want 0 for a fresh failure", result.RetryAfter)
			}
		})
	}
}

func TestStatusCredentialValueErrorMapsInvalidResponse(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	cookiePath := filepath.Join(directory, "cookie")
	if err := os.WriteFile(cookiePath, []byte("sid=ok\r\nX-Leak: yes"), 0o600); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err := os.Chmod(cookiePath, 0o600); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	csrfPath := filepath.Join(directory, "csrf")
	if err := os.WriteFile(csrfPath, []byte("csrf-ok"), 0o600); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err := os.Chmod(csrfPath, 0o600); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	config := DefaultConfig("group-1")
	config.CredentialSource = &credentials.FileCredentialSource{
		CookiePath:    cookiePath,
		CSRFTokenPath: filepath.Join(directory, "csrf"),
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{}
	client.opener = opener

	result, err := client.CheckStatus("0")
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	if result.Status != "invalid_response" || result.AccountType != "unknown" {
		t.Fatalf("status = %+v, want invalid_response for a control-character cookie", result)
	}
	if len(opener.requests) != 0 {
		t.Fatalf("requests = %d, want 0", len(opener.requests))
	}
}

func TestCheckStatusRequiresRootID(t *testing.T) {
	config := DefaultConfig("group-1")
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	if _, err := client.CheckStatus(""); err == nil || err.Error() != "root_id is required" {
		t.Fatalf("error = %v, want root_id is required", err)
	}
}

func TestGroupIDResolution(t *testing.T) {
	config := DefaultConfig("group-1")
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	groupID, err := client.GroupID()
	if err != nil || groupID != "group-1" {
		t.Fatalf("group id = %q err = %v", groupID, err)
	}

	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	pending, err := workspace.NewWorkspaceState(filepath.Join(directory, "missing.json"), "", "")
	if err != nil {
		t.Fatalf("NewWorkspaceState failed: %v", err)
	}
	unresolved := DefaultConfig("")
	unresolved.Workspace = pending
	client, err = NewClient(unresolved)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	_, err = client.GroupID()
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Status != 503 || err.Error() != "WPS operation failed: WPS workspace is not configured (HTTP 503)" {
		t.Fatalf("error = %v, want the 503 unconfigured error", err)
	}

	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	statePath := filepath.Join(directory, "workspace.json")
	if err := os.WriteFile(statePath, []byte(`{"group_id":"ws-group","root_id":"0"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write state failed: %v", err)
	}
	if err := os.Chmod(statePath, 0o600); err != nil {
		t.Fatalf("chmod state failed: %v", err)
	}
	state, err := workspace.NewWorkspaceState(statePath, "", "")
	if err != nil {
		t.Fatalf("NewWorkspaceState failed: %v", err)
	}
	autoConfig := DefaultConfig(workspace.AutoValue)
	autoConfig.Workspace = state
	client, err = NewClient(autoConfig)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	groupID, err = client.GroupID()
	if err != nil || groupID != "ws-group" {
		t.Fatalf("auto group id = %q err = %v, want ws-group", groupID, err)
	}
}

func TestQuotePathSegment(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"group-1", "group-1"},
		{"a+b c", "a%2Bb%20c"},
		{"中文", "%E4%B8%AD%E6%96%87"},
		{"a/b", "a%2Fb"},
		{"safe_.-~", "safe_.-~"},
	}
	for _, test := range tests {
		if got := quotePathSegment(test.value); got != test.want {
			t.Fatalf("quotePathSegment(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestStatusTruthAndAccountType(t *testing.T) {
	truthTests := []struct {
		value any
		want  bool
		known bool
	}{
		{true, true, true},
		{false, false, true},
		{json.Number("1"), true, true},
		{json.Number("0"), false, true},
		{json.Number("2"), true, true},
		{json.Number("1.5"), false, false},
		{"yes", true, true},
		{" LOGGED_OUT ", false, true},
		{"nonsense", false, false},
		{nil, false, false},
		{[]any{}, false, false},
	}
	for index, test := range truthTests {
		got, known := statusTruth(test.value)
		if got != test.want || known != test.known {
			t.Fatalf("case %d statusTruth(%v) = (%v,%v), want (%v,%v)",
				index, test.value, got, known, test.want, test.known)
		}
	}

	typeTests := []struct {
		payload map[string]any
		want    string
	}{
		{map[string]any{"is_company_account": true}, "business"},
		{map[string]any{"is_business_account": json.Number("0")}, "personal"},
		{map[string]any{"is_company_account": "nonsense"}, "unknown"},
		{map[string]any{"companyid": json.Number("691045587")}, "business"},
		{map[string]any{"companyid": json.Number("0")}, "unknown"},
		{map[string]any{"companyid": " 0 "}, "unknown"},
		{map[string]any{"companyid": ""}, "unknown"},
		{map[string]any{"current_companyid": true}, "business"},
		{map[string]any{"current_companyid": false}, "unknown"},
		{map[string]any{"company_id": "company-1"}, "business"},
		{map[string]any{}, "unknown"},
	}
	for index, test := range typeTests {
		if got := statusAccountType(test.payload); got != test.want {
			t.Fatalf("case %d statusAccountType(%v) = %q, want %q",
				index, test.payload, got, test.want)
		}
	}
}

func TestStatusWaiterDeadlineReturnsUnavailable(t *testing.T) {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	config.Timeout = 0.2 // waiter deadline becomes max(0.2, 1) + 1 = 2s; keep the gate closed past it
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &gatedOpener{gate: make(chan struct{}), release: make(chan struct{})}
	client.opener = opener

	leaderDone := make(chan error, 1)
	go func() {
		_, err := client.CheckStatus("0")
		leaderDone <- err
	}()
	<-opener.gate

	waiterDone := make(chan struct{})
	var waiterStatus model.WpsStatus
	go func() {
		result, err := client.CheckStatus("0")
		if err == nil {
			waiterStatus = result
		}
		close(waiterDone)
	}()

	// The leader is blocked; the waiter must hit its deadline and return
	// upstream_unavailable instead of waiting forever.
	select {
	case <-waiterDone:
		if waiterStatus.Status != "upstream_unavailable" || waiterStatus.RetryAfter != 1 {
			t.Fatalf("waiter status = %+v, want upstream_unavailable retry_after 1", waiterStatus)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiter did not time out")
	}
	close(opener.release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader CheckStatus failed: %v", err)
	}
}

var errFakeTransport = fmt.Errorf("fake connection refused")
