package wps

import (
	"strconv"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func listFixtureClient(t *testing.T) (*Client, *fakeControlOpener) {
	t.Helper()
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	opener := &fakeControlOpener{}
	client.opener = opener
	return client, opener
}

func listResponse(body string) scriptedResponse {
	return scriptedResponse{status: 200, body: []byte(body)}
}

func entryJSON(id string, name string, extra string) string {
	return `{"id":` + id + `,"fname":"` + name + `"` + extra + `}`
}

func TestListEntriesBuildsExactV5Request(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{
		listResponse(`{"files":[],"next_offset":-1,"result":"ok"}`),
	}

	page, err := client.ListEntries("folder-1", ListOptions{})
	if err != nil {
		t.Fatalf("ListEntries failed: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Fatalf("entries = %v", page.Entries)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}
	request := opener.requests[0]
	if request.Method != "GET" {
		t.Fatalf("method = %s, want GET", request.Method)
	}
	if request.URL.Path != "/3rd/drive/api/v5/groups/group-1/files" {
		t.Fatalf("path = %q", request.URL.Path)
	}
	wantQuery := "parentid=folder-1&offset=0&count=20&orderby=mtime&order=desc"
	if request.URL.RawQuery != wantQuery {
		t.Fatalf("query = %q, want %q", request.URL.RawQuery, wantQuery)
	}
	if page.Result == nil || *page.Result != "ok" {
		t.Fatalf("result = %v", page.Result)
	}
}

func TestListEntriesOptionalQueryOrder(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{listResponse(`{"files":[]}`)}

	linkGroup := true
	withLink := false
	include := "acl,pic_thumbnail"
	nextFilter := "cursor-1"
	if _, err := client.ListEntries("folder-1", ListOptions{
		GroupID:             "other-group",
		Offset:              40,
		Count:               5,
		Orderby:             "name",
		Order:               "asc",
		LinkGroup:           &linkGroup,
		Include:             &include,
		WithLink:            &withLink,
		ReviewPicThumbnail:  &withLink,
		WithSharefolderType: &linkGroup,
		NextFilter:          &nextFilter,
	}); err != nil {
		t.Fatalf("ListEntries failed: %v", err)
	}
	request := opener.requests[0]
	if request.URL.Path != "/3rd/drive/api/v5/groups/other-group/files" {
		t.Fatalf("path = %q", request.URL.Path)
	}
	wantQuery := "parentid=folder-1&offset=40&count=5&orderby=name&order=asc" +
		"&linkgroup=true&include=acl%2Cpic_thumbnail&with_link=false" +
		"&review_pic_thumbnail=false&with_sharefolder_type=true&next_filter=cursor-1"
	if request.URL.RawQuery != wantQuery {
		t.Fatalf("query = %q, want %q", request.URL.RawQuery, wantQuery)
	}
}

func TestListEntriesParsesEntriesAndCursors(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{listResponse(`{"files":[
		{"id":1,"fname":"one.txt","ftype":"file","parentid":7,"fsize":9,"mtime":100,"fsha":"e1","link_id":"cid-1"},
		{"id":2,"fname":"sub","ftype":"folder"},
		{"id":3,"fname":"weird","ftype":"mystery"}
	],"next_offset":25,"next_filter":"cursor-25","result":"ok"}`)}

	page, err := client.ListEntries("folder-1", ListOptions{})
	if err != nil {
		t.Fatalf("ListEntries failed: %v", err)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("entries = %d, want 3 (unknown kind must not break the page)", len(page.Entries))
	}
	if page.Entries[0].Kind != model.KindFile || page.Entries[1].Kind != model.KindFolder ||
		page.Entries[2].Kind != model.KindUnknown {
		t.Fatalf("kinds = %v/%v/%v", page.Entries[0].Kind, page.Entries[1].Kind, page.Entries[2].Kind)
	}
	if page.NextOffset == nil || *page.NextOffset != 25 {
		t.Fatalf("next offset = %v", page.NextOffset)
	}
	if page.NextFilter == nil || *page.NextFilter != "cursor-25" {
		t.Fatalf("next filter = %v", page.NextFilter)
	}
}

func TestListEntriesSkipsItemsWithoutID(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{listResponse(`{"files":[
		{"fname":"no-id"},
		{"id":null,"fname":"null-id"},
		"not-an-object",
		{"id":4,"fname":"kept.txt"}
	],"next_offset":-1}`)}

	page, err := client.ListEntries("folder-1", ListOptions{})
	if err != nil {
		t.Fatalf("ListEntries failed: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].ID != "4" {
		t.Fatalf("entries = %+v, want only id 4", page.Entries)
	}
}

func TestListEntriesMalformedNameFailsPage(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{listResponse(`{"files":[
		{"id":1,"fname":"ok.txt"},
		{"id":2,"fname":"bad/name"}
	]}`)}

	_, err := client.ListEntries("folder-1", ListOptions{})
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok || apiErr.Operation != "normalize file metadata" {
		t.Fatalf("error = %v, want the metadata error", err)
	}
}

func TestListEntriesFilesShapeAndResult(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"missing files is empty page", `{"next_offset":-1}`, false},
		{"files not a list", `{"files":5}`, true},
		{"files object", `{"files":{}}`, true},
		{"result ok", `{"files":[],"result":"ok"}`, false},
		{"result null", `{"files":[],"result":null}`, false},
		{"result missing", `{"files":[]}`, false},
		{"result error", `{"files":[],"result":"failed"}`, true},
		{"result non string", `{"files":[],"result":5}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, opener := listFixtureClient(t)
			opener.script = []scriptedResponse{listResponse(test.body)}
			_, err := client.ListEntries("folder-1", ListOptions{})
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
			if err != nil {
				apiErr, ok := model.AsWpsAPIError(err)
				if !ok || apiErr.Operation != "list files" || apiErr.Category != model.WpsCategoryUpstream {
					t.Fatalf("error = %v, want list files upstream", err)
				}
			}
		})
	}
}

func TestIterEntriesMultiPageConcatenates(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{
		listResponse(`{"files":[` + entryJSON("1", "a.txt", "") + `],"next_offset":2,"next_filter":"f2","result":"ok"}`),
		listResponse(`{"files":[` + entryJSON("2", "b.txt", "") + `],"next_offset":-1,"result":"ok"}`),
	}

	entries, err := client.IterEntries("folder-1", IterOptions{Count: 1})
	if err != nil {
		t.Fatalf("IterEntries failed: %v", err)
	}
	if len(entries) != 2 || entries[0].ID != "1" || entries[1].ID != "2" {
		t.Fatalf("entries = %+v", entries)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(opener.requests))
	}
	if !strings.Contains(opener.requests[1].URL.RawQuery, "offset=2") ||
		!strings.Contains(opener.requests[1].URL.RawQuery, "next_filter=f2") {
		t.Fatalf("second page query = %q", opener.requests[1].URL.RawQuery)
	}
}

func TestIterEntriesEmptyFolder(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{listResponse(`{"files":[],"next_offset":-1,"result":"ok"}`)}

	entries, err := client.IterEntries("folder-1", IterOptions{Count: 100})
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries = %v err = %v", entries, err)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(opener.requests))
	}
}

func TestIterEntriesDedupesBoundaryOverlaps(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{
		listResponse(`{"files":[` + entryJSON("1", "a.txt", "") + `],"next_offset":10,"result":"ok"}`),
		listResponse(`{"files":[` + entryJSON("1", "a.txt", "") + "," + entryJSON("2", "b.txt", "") + `],"next_offset":-1,"result":"ok"}`),
	}

	entries, err := client.IterEntries("folder-1", IterOptions{Count: 1})
	if err != nil {
		t.Fatalf("IterEntries failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want overlap deduplicated", entries)
	}
	if entries[0].ID != "1" || entries[1].ID != "2" {
		t.Fatalf("entries order = %+v", entries)
	}
}

func TestIterEntriesRepeatedCursorStops(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{
		listResponse(`{"files":[` + entryJSON("1", "a.txt", "") + `],"next_offset":2,"result":"ok"}`),
		listResponse(`{"files":[],"next_offset":2,"result":"ok"}`),
	}

	entries, err := client.IterEntries("folder-1", IterOptions{Count: 1})
	if err != nil {
		t.Fatalf("IterEntries failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	if len(opener.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (loop stopped on the repeated cursor)", len(opener.requests))
	}
}

func TestIterEntriesNonAdvancingCursorStops(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{
		listResponse(`{"files":[` + entryJSON("1", "a.txt", "") + `],"next_offset":0,"result":"ok"}`),
	}

	entries, err := client.IterEntries("folder-1", IterOptions{Count: 1})
	if err != nil {
		t.Fatalf("IterEntries failed: %v", err)
	}
	if len(entries) != 1 || len(opener.requests) != 1 {
		t.Fatalf("entries = %+v requests = %d, want a single page", entries, len(opener.requests))
	}
}

func TestIterEntriesInsufficientStorageOnEntryLimit(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{
		listResponse(`{"files":[` + entryJSON("1", "a.txt", "") + "," + entryJSON("2", "b.txt", "") + `],"next_offset":10,"result":"ok"}`),
		listResponse(`{"files":[` + entryJSON("3", "c.txt", "") + `],"next_offset":-1,"result":"ok"}`),
	}
	maxEntries := 2
	_, err := client.IterEntries("folder-1", IterOptions{Count: 2, MaxEntries: &maxEntries})
	storageErr, ok := model.AsStorageError(err)
	if !ok || storageErr.Kind != model.KindInsufficientStorage ||
		storageErr.Message != "remote folder exceeds the configured entry limit" {
		t.Fatalf("error = %v, want the entry-limit insufficient storage error", err)
	}
}

func TestIterEntriesPageLimitExceeded(t *testing.T) {
	client, opener := listFixtureClient(t)
	script := []scriptedResponse{}
	for index := 0; index < 4; index++ {
		script = append(script, listResponse(
			`{"files":[],"next_offset":`+strconv.Itoa(index+1)+`,"result":"ok"}`))
	}
	opener.script = script
	maxEntries := 3
	_, err := client.IterEntries("folder-1", IterOptions{Count: 1, MaxEntries: &maxEntries})
	storageErr, ok := model.AsStorageError(err)
	if !ok || storageErr.Kind != model.KindInsufficientStorage ||
		storageErr.Message != "remote folder pagination exceeds the configured limit" {
		t.Fatalf("error = %v, want the pagination-limit insufficient storage error", err)
	}
}

func TestIterEntriesMaxEntriesExactBoundarySucceeds(t *testing.T) {
	client, opener := listFixtureClient(t)
	opener.script = []scriptedResponse{
		listResponse(`{"files":[` + entryJSON("1", "a.txt", "") + `],"next_offset":2,"result":"ok"}`),
		listResponse(`{"files":[` + entryJSON("2", "b.txt", "") + `],"next_offset":-1,"result":"ok"}`),
	}
	maxEntries := 2
	entries, err := client.IterEntries("folder-1", IterOptions{Count: 1, MaxEntries: &maxEntries})
	if err != nil {
		t.Fatalf("IterEntries failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want exactly the limit", entries)
	}
}

func TestIterEntriesValidatesArguments(t *testing.T) {
	client, _ := listFixtureClient(t)
	if _, err := client.IterEntries("folder-1", IterOptions{}); err == nil ||
		err.Error() != "count must be positive" {
		t.Fatalf("error = %v, want count must be positive", err)
	}
	zero := 0
	if _, err := client.IterEntries("folder-1", IterOptions{Count: 1, MaxEntries: &zero}); err == nil ||
		err.Error() != "max_entries must be positive" {
		t.Fatalf("error = %v, want max_entries must be positive", err)
	}
}
