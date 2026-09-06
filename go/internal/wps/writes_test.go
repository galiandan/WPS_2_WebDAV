// The create_folder tests replay the confirmed JSON body from the WPS UI
// capture and pin every failure surface: bad names, missing CSRF, failed
// result fields, HTTP and transport errors, and the 401 retry with rotated
// credentials.

package wps

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func newWriteClient(t *testing.T, opener Opener, mutate func(*Config)) *Client {
	t.Helper()
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	if mutate != nil {
		mutate(&config)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.opener = opener
	return client
}

func TestCreateFolderSendsConfirmedJSONBody(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(
			`{"id":9,"fname":"new-folder","ftype":"folder","parentid":3,"fsize":0,"result":"ok"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	entry, err := client.CreateFolder("3", "new-folder")
	if err != nil {
		t.Fatalf("CreateFolder failed: %v", err)
	}
	if entry.ID != "9" || entry.Kind != model.KindFolder || entry.Name != "new-folder" {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.ParentID == nil || *entry.ParentID != "3" {
		t.Fatalf("parent id = %v, want 3", entry.ParentID)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}
	request := opener.requests[0]
	if request.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", request.Method)
	}
	if request.URL.String() != "https://365.kdocs.cn/3rd/drive/api/v5/files/folder" {
		t.Fatalf("url = %q", request.URL.String())
	}
	if request.Header.Get("Cookie") != "Cookie-secret" {
		t.Fatalf("cookie = %q", request.Header.Get("Cookie"))
	}
	if request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", request.Header.Get("Content-Type"))
	}
	// The body is compared byte for byte: key order, compact separators,
	// and ensure_ascii escapes are all part of the captured request shape.
	wantBody := `{"groupid":1,"parentid":3,"name":"new-folder","owner":true,"parsed":true,"csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[0]) != wantBody {
		t.Fatalf("body = %q, want %q", opener.bodies[0], wantBody)
	}
}

func TestCreateFolderKeepsNonDecimalIDsAsStrings(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"id":9,"fname":"new-folder","ftype":"folder","result":"ok"}`)},
	}}
	client := newWriteClient(t, opener, nil)

	if _, err := client.CreateFolder("abc123", "new-folder"); err != nil {
		t.Fatalf("CreateFolder failed: %v", err)
	}
	wantBody := `{"groupid":"group-1","parentid":"abc123","name":"new-folder","owner":true,"parsed":true,"csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[0]) != wantBody {
		t.Fatalf("body = %q, want %q", opener.bodies[0], wantBody)
	}
}

func TestCreateFolderNormalizesLeadingZerosLikePythonInt(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"id":9,"fname":"new-folder","ftype":"folder","result":"ok"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "007" })

	if _, err := client.CreateFolder("000", "new-folder"); err != nil {
		t.Fatalf("CreateFolder failed: %v", err)
	}
	wantBody := `{"groupid":7,"parentid":0,"name":"new-folder","owner":true,"parsed":true,"csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[0]) != wantBody {
		t.Fatalf("body = %q, want %q", opener.bodies[0], wantBody)
	}
}

func TestCreateFolderEscapesNonASCIIName(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"id":9,"fname":"中文","ftype":"folder","result":"ok"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	entry, err := client.CreateFolder("3", "中文")
	if err != nil {
		t.Fatalf("CreateFolder failed: %v", err)
	}
	if entry.Name != "中文" {
		t.Fatalf("name = %q", entry.Name)
	}
	if !strings.Contains(string(opener.bodies[0]), `\u4e2d\u6587`) {
		t.Fatalf("body = %q, want escaped unicode", opener.bodies[0])
	}
}

func TestCreateFolderRejectsBadNamesBeforeAnyRequest(t *testing.T) {
	opener := &fakeControlOpener{}
	client := newWriteClient(t, opener, nil)

	for _, name := range []string{"", "a/b", "a\\b", "/", "\\"} {
		if _, err := client.CreateFolder("3", name); err == nil ||
			err.Error() != "name must be one remote folder name" {
			t.Fatalf("CreateFolder(%q) error = %v, want the ValueError message", name, err)
		}
	}
	if len(opener.requests) != 0 {
		t.Fatalf("requests = %d, want 0", len(opener.requests))
	}
}

func TestCreateFolderRequiresCSRF(t *testing.T) {
	opener := &fakeControlOpener{}
	client := newWriteClient(t, opener, func(c *Config) {
		c.GroupID = "1"
		c.CredentialSource = &credentials.StaticCredentialSource{
			Credentials: credentials.Credentials{Cookie: "Cookie-secret"},
		}
	})

	if _, err := client.CreateFolder("3", "new-folder"); err == nil ||
		err.Error() != "csrf_token is required for write operation" {
		t.Fatalf("error = %v, want the missing-csrf ValueError message", err)
	}
	if len(opener.requests) != 0 {
		t.Fatalf("requests = %d, want 0", len(opener.requests))
	}
}

func TestCreateFolderRejectsFailedResult(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"error"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	_, err := client.CreateFolder("3", "new-folder")
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Operation != "create folder" || apiErr.Status != 0 ||
		apiErr.Category != model.WpsCategoryUpstream {
		t.Fatalf("error = %v, want the upstream create-folder failure", err)
	}
	if err.Error() != "WPS operation failed: create folder" {
		t.Fatalf("message = %q, want the operation-only text", err.Error())
	}
}

func TestCreateFolderToleratesMissingResult(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"id":9,"fname":"new-folder","ftype":"folder"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	entry, err := client.CreateFolder("3", "new-folder")
	if err != nil {
		t.Fatalf("CreateFolder failed: %v", err)
	}
	if entry.ID != "9" {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestCreateFolderMapsHTTPAndTransportErrors(t *testing.T) {
	cases := []struct {
		name     string
		script   []scriptedResponse
		failures []error
		status   int
		category string
	}{
		{
			name:     "permission-failure",
			script:   []scriptedResponse{{status: 403}},
			status:   403,
			category: model.WpsCategoryHTTP,
		},
		{
			name:     "transport-failure",
			failures: []error{errors.New("connection refused")},
			status:   0,
			category: model.WpsCategoryUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opener := &fakeControlOpener{script: tc.script, failures: tc.failures}
			client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

			_, err := client.CreateFolder("3", "new-folder")
			apiErr, ok := model.AsWpsAPIError(err)
			if !ok || apiErr.Status != tc.status || apiErr.Category != tc.category {
				t.Fatalf("error = %v, want status %d category %s", err, tc.status, tc.category)
			}
		})
	}
}

func TestCreateFolder401RetriesWithRotatedCredentials(t *testing.T) {
	directory, source := writeCredentialFiles(t, "sid=first", "csrf-first")
	config := DefaultConfig("1")
	config.CredentialSource = source
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 401},
		{status: 200, body: []byte(
			`{"id":2,"fname":"new-folder","ftype":"folder","parentid":1,"fsize":0,"mtime":2,"result":"ok"}`)},
	}}
	rotate := func() {
		if err := os.WriteFile(filepath.Join(directory, "cookie"), []byte("sid=second"), 0o600); err != nil {
			t.Fatalf("rotate cookie failed: %v", err)
		}
		if err := os.WriteFile(filepath.Join(directory, "csrf"), []byte("csrf-second"), 0o600); err != nil {
			t.Fatalf("rotate csrf failed: %v", err)
		}
	}
	client.opener = &rotatingOpener{inner: opener, before: rotate}

	entry, err := client.CreateFolder("1", "new-folder")
	if err != nil {
		t.Fatalf("CreateFolder failed: %v", err)
	}
	if entry.ID != "2" {
		t.Fatalf("entry = %+v", entry)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(opener.requests))
	}
	retry := opener.requests[1]
	if retry.Header.Get("Cookie") != "sid=second" {
		t.Fatalf("retry cookie = %q, want sid=second", retry.Header.Get("Cookie"))
	}
	wantBody := `{"groupid":1,"parentid":1,"name":"new-folder","owner":true,"parsed":true,"csrfmiddlewaretoken":"csrf-second"}`
	if string(opener.bodies[1]) != wantBody {
		t.Fatalf("retry body = %q, want %q", opener.bodies[1], wantBody)
	}
}

func TestCreateFolderRejectsMalformedEntry(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	_, err := client.CreateFolder("3", "new-folder")
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Operation != "normalize file metadata" {
		t.Fatalf("error = %v, want the metadata-normalization failure", err)
	}
}

func TestPyJSONID(t *testing.T) {
	cases := []struct {
		value string
		want  any
	}{
		{"1", json.Number("1")},
		{"007", json.Number("7")},
		{"0", json.Number("0")},
		{"000", json.Number("0")},
		{"123456789012345678901234567890", json.Number("123456789012345678901234567890")},
		{"group-1", "group-1"},
		{"12a", "12a"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := pyJSONID(tc.value); got != tc.want {
			t.Errorf("pyJSONID(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
