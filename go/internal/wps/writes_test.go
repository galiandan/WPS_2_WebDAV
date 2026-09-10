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

func TestRenameSendsConfirmedV3Body(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(
			`{"id":9,"fname":"renamed-folder","ftype":"folder","groupid":1,"parentid":3,"fsize":0,"mtime":123}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	entry, err := client.Rename("9", "renamed-folder")
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}
	if entry.ID != "9" || entry.Name != "renamed-folder" || entry.Kind != model.KindFolder {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.ModifiedAt == nil || *entry.ModifiedAt != "123" {
		t.Fatalf("mtime = %v, want 123", entry.ModifiedAt)
	}
	request := opener.requests[0]
	if request.Method != http.MethodPut {
		t.Fatalf("method = %s, want PUT", request.Method)
	}
	if request.URL.Path != "/3rd/drive/api/v3/groups/1/files/9" {
		t.Fatalf("path = %q", request.URL.Path)
	}
	if request.Header.Get("Cookie") != "Cookie-secret" {
		t.Fatalf("cookie = %q", request.Header.Get("Cookie"))
	}
	wantBody := `{"fname":"renamed-folder","csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[0]) != wantBody {
		t.Fatalf("body = %q, want %q", opener.bodies[0], wantBody)
	}
}

func TestRenameQuotesGroupAndFileID(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"id":"f 9","fname":"renamed","ftype":"folder"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "g/1" })

	if _, err := client.Rename("f 9", "renamed"); err != nil {
		t.Fatalf("Rename failed: %v", err)
	}
	want := "/3rd/drive/api/v3/groups/g%2F1/files/f%209"
	if escaped := opener.requests[0].URL.EscapedPath(); escaped != want {
		t.Fatalf("path = %q, want %q", escaped, want)
	}
}

func TestRenameRejectsBadArgumentsBeforeAnyRequest(t *testing.T) {
	opener := &fakeControlOpener{}
	client := newWriteClient(t, opener, nil)

	if _, err := client.Rename("", "renamed"); err == nil || err.Error() != "file_id is required" {
		t.Fatalf("empty file_id error = %v, want the ValueError message", err)
	}
	for _, name := range []string{"", "a/b", "a\\b", "/"} {
		if _, err := client.Rename("9", name); err == nil ||
			err.Error() != "name must be one remote entry name" {
			t.Fatalf("Rename name %q error = %v, want the ValueError message", name, err)
		}
	}
	if len(opener.requests) != 0 {
		t.Fatalf("requests = %d, want 0", len(opener.requests))
	}
}

func TestRenameRejectsFailedResult(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"error"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	_, err := client.Rename("9", "renamed")
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Operation != "rename file" || apiErr.Status != 0 ||
		apiErr.Category != model.WpsCategoryUpstream {
		t.Fatalf("error = %v, want the upstream rename failure", err)
	}
	if err.Error() != "WPS operation failed: rename file" {
		t.Fatalf("message = %q, want the operation-only text", err.Error())
	}
}

func TestRenameMapsHTTPAndTransportErrors(t *testing.T) {
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

			_, err := client.Rename("9", "renamed")
			apiErr, ok := model.AsWpsAPIError(err)
			if !ok || apiErr.Status != tc.status || apiErr.Category != tc.category {
				t.Fatalf("error = %v, want status %d category %s", err, tc.status, tc.category)
			}
		})
	}
}

func TestMovePostsTaskAndWaitsForSuccess(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok","taskid":13,"taskuuid":"move-task"}`)},
		{status: 200, body: []byte(
			`{"estimated_time_left":-1,"failed_list":null,"finish":1,` +
				`"result":"ok","status":"success","taskid":13,` +
				`"taskuuid":"move-task","total":1}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	if err := client.Move("7", "3", "8"); err != nil {
		t.Fatalf("Move failed: %v", err)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(opener.requests))
	}
	moveRequest := opener.requests[0]
	if moveRequest.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", moveRequest.Method)
	}
	if moveRequest.URL.Path != "/3rd/drive/api/v5/files/batch/task/move" {
		t.Fatalf("path = %q", moveRequest.URL.Path)
	}
	if moveRequest.Header.Get("Cookie") != "Cookie-secret" {
		t.Fatalf("cookie = %q", moveRequest.Header.Get("Cookie"))
	}
	// Byte-for-byte: field order, the empty option dict, the numeric id
	// list, and the ensure_ascii CSRF token are part of the captured body.
	wantBody := `{"groupid":1,"parentid":3,"dst_groupid":1,"dst_parentid":8,` +
		`"fileids":[7],"option":{},"csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[0]) != wantBody {
		t.Fatalf("body = %q, want %q", opener.bodies[0], wantBody)
	}
	progressRequest := opener.requests[1]
	if progressRequest.Method != http.MethodGet || progressRequest.URL.Path != "/3rd/drive/api/v5/files/batch/task/progress" {
		t.Fatalf("progress request = %s %s", progressRequest.Method, progressRequest.URL.Path)
	}
	if query := progressRequest.URL.Query(); query.Get("taskuuid") != "move-task" {
		t.Fatalf("progress query = %v", query)
	}
}

func TestPersonalMoveUsesDirectV3Endpoint(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) {
		c.GroupID = "1"
		c.Mode = ModePersonal
	})
	if err := client.Move("7", "3", "8"); err != nil {
		t.Fatalf("personal Move failed: %v", err)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}
	request := opener.requests[0]
	if request.URL.Host != "drive.wps.cn" || request.URL.Path != "/api/v3/groups/1/files/batch/move" {
		t.Fatalf("url = %q", request.URL.String())
	}
	wantBody := `{"fileids":[7],"target_groupid":1,"target_parentid":8}`
	if string(opener.bodies[0]) != wantBody {
		t.Fatalf("body = %q, want %q", opener.bodies[0], wantBody)
	}
}

func TestMoveRejectsBadArgumentsBeforeAnyRequest(t *testing.T) {
	opener := &fakeControlOpener{}
	client := newWriteClient(t, opener, nil)

	if err := client.Move("", "3", "8"); err == nil || err.Error() != "file_id is required" {
		t.Fatalf("empty file_id error = %v, want the ValueError message", err)
	}
	if err := client.Move("7", "", "8"); err == nil ||
		err.Error() != "source and destination parent IDs are required" {
		t.Fatalf("empty source error = %v, want the ValueError message", err)
	}
	if err := client.Move("7", "3", ""); err == nil ||
		err.Error() != "source and destination parent IDs are required" {
		t.Fatalf("empty destination error = %v, want the ValueError message", err)
	}
	if len(opener.requests) != 0 {
		t.Fatalf("requests = %d, want 0", len(opener.requests))
	}
}

func TestMoveRejectsFailedTaskResult(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"error"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	err := client.Move("7", "3", "8")
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Operation != "move file" || apiErr.Status != 0 ||
		apiErr.Category != model.WpsCategoryUpstream {
		t.Fatalf("error = %v, want the upstream move failure", err)
	}
}

func TestMoveRejectsMalformedTaskUUID(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing", `{"result":"ok"}`},
		{"empty", `{"result":"ok","taskuuid":""}`},
		{"number", `{"result":"ok","taskuuid":12}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opener := &fakeControlOpener{script: []scriptedResponse{
				{status: 200, body: []byte(tc.body)},
			}}
			client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

			err := client.Move("7", "3", "8")
			apiErr, ok := model.AsWpsAPIError(err)
			if !ok || apiErr.Operation != "move file task" || apiErr.Status != 0 ||
				apiErr.Category != model.WpsCategoryUpstream {
				t.Fatalf("error = %v, want the move-file task failure", err)
			}
		})
	}
}

func TestMoveWaitsForObservedTaskBeforeReturning(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok","taskuuid":"move-task"}`)},
		{status: 200, body: []byte(`{"finish":0,"result":"ok","status":"failed"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	err := client.Move("7", "3", "8")
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Operation != "move file task" || apiErr.Status != 0 {
		t.Fatalf("error = %v, want the observed task failure", err)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want the progress poll to run", len(opener.requests))
	}
}

func TestDeletePostsTaskAndWaitsForSuccess(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok","taskid":12,"taskuuid":"task-uuid"}`)},
		{status: 200, body: []byte(
			`{"estimated_time_left":-1,"failed_list":null,"finish":1,` +
				`"result":"ok","status":"success","taskid":12,` +
				`"taskuuid":"task-uuid","total":1}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	if err := client.Delete("7"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(opener.requests))
	}
	deleteRequest := opener.requests[0]
	if deleteRequest.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", deleteRequest.Method)
	}
	if deleteRequest.URL.Path != "/3rd/drive/api/v5/files/batch/task/delete" {
		t.Fatalf("path = %q", deleteRequest.URL.Path)
	}
	if deleteRequest.Header.Get("Cookie") != "Cookie-secret" {
		t.Fatalf("cookie = %q", deleteRequest.Header.Get("Cookie"))
	}
	wantBody := `{"fileids":[7],"groupid":1,"csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[0]) != wantBody {
		t.Fatalf("body = %q, want %q", opener.bodies[0], wantBody)
	}
	progressRequest := opener.requests[1]
	if progressRequest.Method != http.MethodGet ||
		progressRequest.URL.Path != "/3rd/drive/api/v5/files/batch/task/progress" {
		t.Fatalf("progress request = %s %s", progressRequest.Method, progressRequest.URL.Path)
	}
	if query := progressRequest.URL.Query(); query.Get("taskuuid") != "task-uuid" {
		t.Fatalf("progress query = %v", query)
	}
}

func TestPersonalDeleteUsesDirectV3Endpoint(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) {
		c.GroupID = "1"
		c.Mode = ModePersonal
	})
	if err := client.Delete("7"); err != nil {
		t.Fatalf("personal Delete failed: %v", err)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}
	request := opener.requests[0]
	if request.URL.Host != "drive.wps.cn" || request.URL.Path != "/api/v3/groups/1/files/batch/delete" {
		t.Fatalf("url = %q", request.URL.String())
	}
	wantBody := `{"fileids":[7],"groupid":1,"csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[0]) != wantBody {
		t.Fatalf("body = %q, want %q", opener.bodies[0], wantBody)
	}
}

func TestDeleteRejectsEmptyFileIDBeforeAnyRequest(t *testing.T) {
	opener := &fakeControlOpener{}
	client := newWriteClient(t, opener, nil)

	if err := client.Delete(""); err == nil || err.Error() != "file_id is required" {
		t.Fatalf("error = %v, want the ValueError message", err)
	}
	if len(opener.requests) != 0 {
		t.Fatalf("requests = %d, want 0", len(opener.requests))
	}
}

func TestDeleteRejectsFailedTaskResult(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"error"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	err := client.Delete("7")
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Operation != "delete file" || apiErr.Status != 0 ||
		apiErr.Category != model.WpsCategoryUpstream {
		t.Fatalf("error = %v, want the upstream delete failure", err)
	}
}

func TestDeleteRejectsMalformedTaskUUID(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing", `{"result":"ok"}`},
		{"empty", `{"result":"ok","taskuuid":""}`},
		{"number", `{"result":"ok","taskuuid":12}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opener := &fakeControlOpener{script: []scriptedResponse{
				{status: 200, body: []byte(tc.body)},
			}}
			client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

			err := client.Delete("7")
			apiErr, ok := model.AsWpsAPIError(err)
			if !ok || apiErr.Operation != "delete file task" || apiErr.Status != 0 ||
				apiErr.Category != model.WpsCategoryUpstream {
				t.Fatalf("error = %v, want the delete-file task failure", err)
			}
		})
	}
}

func TestDeleteWaitsForObservedTaskBeforeReturning(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok","taskuuid":"task-uuid"}`)},
		{status: 200, body: []byte(`{"finish":0,"result":"ok","status":"failed"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	err := client.Delete("7")
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Operation != "delete file task" || apiErr.Status != 0 {
		t.Fatalf("error = %v, want the observed task failure", err)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want the progress poll to run", len(opener.requests))
	}
}

func TestCopySendsConfirmedV3BatchBody(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok","fileids":["bench-file-copied"]}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	copied, err := client.Copy("7", "3")
	if err != nil {
		t.Fatalf("Copy failed: %v", err)
	}
	if copied != "bench-file-copied" {
		t.Fatalf("copied id = %q", copied)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}
	request := opener.requests[0]
	if request.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", request.Method)
	}
	if request.URL.String() != "https://365.kdocs.cn/3rd/drive/api/v3/groups/1/files/batch/copy" {
		t.Fatalf("url = %q", request.URL.String())
	}
	// Byte-for-byte: field order, duplicated_name_model as a JSON number,
	// and the numeric id list are part of the captured request shape.
	wantBody := `{"fileids":[7],"groupid":1,"target_groupid":1,"target_parentid":3,"duplicated_name_model":1,"csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[0]) != wantBody {
		t.Fatalf("body = %q, want %q", opener.bodies[0], wantBody)
	}
}

func TestCopyQuotesGroupInURLAndKeepsNonDecimalIDsAsStrings(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok","fileids":["copied-abc"]}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "g/1" })

	if _, err := client.Copy("file/7", "3"); err != nil {
		t.Fatalf("Copy failed: %v", err)
	}
	wantURL := "https://365.kdocs.cn/3rd/drive/api/v3/groups/g%2F1/files/batch/copy"
	if opener.requests[0].URL.String() != wantURL {
		t.Fatalf("url = %q, want %q", opener.requests[0].URL.String(), wantURL)
	}
	wantBody := `{"fileids":["file/7"],"groupid":"g/1","target_groupid":"g/1","target_parentid":3,"duplicated_name_model":1,"csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[0]) != wantBody {
		t.Fatalf("body = %q, want %q", opener.bodies[0], wantBody)
	}
}

func TestCopyRejectsMissingIDsWithoutRequests(t *testing.T) {
	client := newWriteClient(t, &fakeControlOpener{}, nil)
	if _, err := client.Copy("", "3"); err == nil || err.Error() != "file and target parent IDs are required" {
		t.Fatalf("empty file id error = %v", err)
	}
	if _, err := client.Copy("7", ""); err == nil || err.Error() != "file and target parent IDs are required" {
		t.Fatalf("empty parent error = %v", err)
	}
}

func TestCopyResultFailureAndMissingFileID(t *testing.T) {
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"failed"}`)},
		{status: 200, body: []byte(`{"result":"ok","fileids":[]}`)},
		{status: 200, body: []byte(`{"result":"ok"}`)},
		{status: 200, body: []byte(`{"result":"ok","fileids":[1,2]}`)},
		{status: 200, body: []byte(`{"result":"ok","fileids":"bench"}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	for index := 0; index < 5; index++ {
		if _, err := client.Copy("7", "3"); err == nil {
			t.Fatalf("case %d: expected an error", index)
		} else if index == 0 {
			if _, ok := model.AsWpsAPIError(err); !ok {
				t.Fatalf("case 0 error type = %T", err)
			}
		}
	}
	if len(opener.requests) != 5 {
		t.Fatalf("requests = %d, want 5", len(opener.requests))
	}
}

func TestCopyRejectsInvalidFileIDShapes(t *testing.T) {
	// Python's gate is isinstance(copied, (str, int)) after excluding bool:
	// floats, nulls, and containers are invalid; a JSON integer normalizes
	// through str(int(...)).
	opener := &fakeControlOpener{script: []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok","fileids":[true]}`)},
		{status: 200, body: []byte(`{"result":"ok","fileids":[1.5]}`)},
		{status: 200, body: []byte(`{"result":"ok","fileids":[1e3]}`)},
		{status: 200, body: []byte(`{"result":"ok","fileids":[null]}`)},
		{status: 200, body: []byte(`{"result":"ok","fileids":[[7]]}`)},
		{status: 200, body: []byte(`{"result":"ok","fileids":[7]}`)},
		{status: 200, body: []byte(`{"result":"ok","fileids":[-0]}`)},
	}}
	client := newWriteClient(t, opener, func(c *Config) { c.GroupID = "1" })

	for index := 0; index < 5; index++ {
		if _, err := client.Copy("7", "3"); err == nil || err.Error() != "WPS operation failed: copy response contains invalid file ID" {
			t.Fatalf("case %d error = %v", index, err)
		}
	}
	copied, err := client.Copy("7", "3")
	if err != nil || copied != "7" {
		t.Fatalf("integer id = (%q, %v)", copied, err)
	}
	copied, err = client.Copy("7", "3")
	if err != nil || copied != "0" {
		t.Fatalf("-0 id = (%q, %v)", copied, err)
	}
}
