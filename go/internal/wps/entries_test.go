package wps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func mustDecodeItem(t *testing.T, text string) map[string]any {
	t.Helper()
	decoded, err := decodeJSONObject([]byte(text))
	if err != nil {
		t.Fatalf("fixture decode failed: %v", err)
	}
	return decoded
}

func normalizeItem(t *testing.T, text string) (model.RemoteEntry, error) {
	t.Helper()
	return entryFromItem(mustDecodeItem(t, text))
}

func TestEntryFromItemFullShape(t *testing.T) {
	entry, err := normalizeItem(t, `{
		"id": 123,
		"fname": "bench.txt",
		"ftype": "file",
		"parentid": 3,
		"fsize": 4096,
		"mtime": 1788600000,
		"fsha": "\"etag-value\"",
		"link_id": "download-cid",
		"signed_url": "https://hwc-bj.ag.kdocs.cn/signed?sig=fake"
	}`)
	if err != nil {
		t.Fatalf("entryFromItem failed: %v", err)
	}
	if entry.ID != "123" || entry.Name != "bench.txt" || entry.Kind != model.KindFile {
		t.Fatalf("identity = %+v", entry)
	}
	if entry.ParentID == nil || *entry.ParentID != "3" {
		t.Fatalf("parent id = %v", entry.ParentID)
	}
	if entry.Size == nil || *entry.Size != 4096 {
		t.Fatalf("size = %v", entry.Size)
	}
	if entry.ModifiedAt == nil || *entry.ModifiedAt != "1788600000" {
		t.Fatalf("mtime = %v", entry.ModifiedAt)
	}
	if entry.Etag == nil || *entry.Etag != `"etag-value"` {
		t.Fatalf("etag = %v", entry.Etag)
	}
	if entry.LinkID == nil || *entry.LinkID != "download-cid" {
		t.Fatalf("link id = %v", entry.LinkID)
	}
	if entry.Raw == nil || entry.Raw["signed_url"] == nil {
		t.Fatal("raw payload must stay available for later WPS operations")
	}
	public := entry.Public()
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	text := string(encoded)
	if strings.Contains(text, "download-cid") || strings.Contains(text, "signed") {
		t.Fatalf("public projection leaks internal fields: %s", text)
	}
}

func TestEntryFromItemRequiresID(t *testing.T) {
	for _, item := range []string{
		`{"fname":"a.txt"}`,
		`{"id":null,"fname":"a.txt"}`,
		`{}`,
	} {
		_, err := normalizeItem(t, item)
		apiErr, ok := model.AsWpsAPIError(err)
		if !ok || apiErr.Operation != "normalize file metadata" || apiErr.Status != 0 ||
			apiErr.Category != model.WpsCategoryUpstream {
			t.Fatalf("item %s error = %v, want the fixed metadata error", item, err)
		}
		if strings.Contains(err.Error(), "a.txt") {
			t.Fatalf("error leaks item content: %q", err.Error())
		}
	}
}

func TestEntryFromItemStringifiesID(t *testing.T) {
	tests := []struct {
		item string
		want string
	}{
		{`{"id":123,"fname":"a.txt"}`, "123"},
		{`{"id":"abc","fname":"a.txt"}`, "abc"},
		{`{"id":1.0,"fname":"a.txt"}`, "1.0"},
		{`{"id":true,"fname":"a.txt"}`, "True"},
	}
	for _, test := range tests {
		entry, err := normalizeItem(t, test.item)
		if err != nil {
			t.Fatalf("item %s failed: %v", test.item, err)
		}
		if entry.ID != test.want {
			t.Fatalf("item %s id = %q, want %q", test.item, entry.ID, test.want)
		}
	}
}

func TestEntryFromItemNameValidation(t *testing.T) {
	rejects := []string{
		`{"id":1}`,
		`{"id":1,"fname":""}`,
		`{"id":1,"fname":5}`,
		`{"id":1,"fname":null}`,
		`{"id":1,"fname":"."}`,
		`{"id":1,"fname":".."}`,
		`{"id":1,"fname":"a/b"}`,
		`{"id":1,"fname":"a\\b"}`,
		`{"id":1,"fname":"a\u0000b"}`,
		`{"id":1,"fname":"a\u001fb"}`,
		`{"id":1,"fname":"a\u007fb"}`,
		`{"id":1,"fname":"` + strings.Repeat("a", 4097) + `"}`,
	}
	for _, item := range rejects {
		if _, err := normalizeItem(t, item); err == nil {
			t.Fatalf("item %.60s accepted", item)
		}
	}
	accepts := []string{
		`{"id":1,"fname":"` + strings.Repeat("a", 4096) + `"}`,
		`{"id":1,"fname":"` + strings.Repeat("中", 1365) + `a"}`,
		`{"id":1,"fname":"中文 emoji 😀.txt"}`,
	}
	for _, item := range accepts {
		if _, err := normalizeItem(t, item); err != nil {
			t.Fatalf("item %.40s rejected: %v", item, err)
		}
	}
}

func TestEntryFromItemKindNormalization(t *testing.T) {
	tests := []struct {
		item string
		want model.EntryKind
	}{
		{`{"id":1,"fname":"a","ftype":"file"}`, model.KindFile},
		{`{"id":1,"fname":"a","ftype":"folder"}`, model.KindFolder},
		{`{"id":1,"fname":"a","ftype":"weird"}`, model.KindUnknown},
		{`{"id":1,"fname":"a","ftype":5}`, model.KindUnknown},
		{`{"id":1,"fname":"a"}`, model.KindUnknown},
	}
	for _, test := range tests {
		entry, err := normalizeItem(t, test.item)
		if err != nil {
			t.Fatalf("item %s failed: %v", test.item, err)
		}
		if entry.Kind != test.want {
			t.Fatalf("item %s kind = %v, want %v", test.item, entry.Kind, test.want)
		}
	}
}

func TestEntryFromItemSizeNormalization(t *testing.T) {
	tests := []struct {
		item string
		want *int64
	}{
		{`{"id":1,"fname":"a"}`, nil},
		{`{"id":1,"fname":"a","fsize":null}`, nil},
		{`{"id":1,"fname":"a","fsize":"5"}`, nil},
		{`{"id":1,"fname":"a","fsize":true}`, nil},
		{`{"id":1,"fname":"a","fsize":-1}`, nil},
		{`{"id":1,"fname":"a","fsize":5.5}`, nil},
	}
	for _, test := range tests {
		entry, err := normalizeItem(t, test.item)
		if err != nil {
			t.Fatalf("item %s failed: %v", test.item, err)
		}
		if (entry.Size == nil) != (test.want == nil) {
			t.Fatalf("item %s size = %v, want %v", test.item, entry.Size, test.want)
		}
	}
	entry, err := normalizeItem(t, `{"id":1,"fname":"a","fsize":0}`)
	if err != nil || entry.Size == nil || *entry.Size != 0 {
		t.Fatalf("zero size = %+v err = %v, want kept", entry.Size, err)
	}
}

func TestEntryFromItemMtimeNormalization(t *testing.T) {
	missing, err := normalizeItem(t, `{"id":1,"fname":"a"}`)
	if err != nil || missing.ModifiedAt != nil {
		t.Fatalf("missing mtime = %v err = %v", missing.ModifiedAt, err)
	}
	nullEntry, err := normalizeItem(t, `{"id":1,"fname":"a","mtime":null}`)
	if err != nil || nullEntry.ModifiedAt != nil {
		t.Fatalf("null mtime = %v err = %v", nullEntry.ModifiedAt, err)
	}
	numeric, err := normalizeItem(t, `{"id":1,"fname":"a","mtime":1788600000}`)
	if err != nil || numeric.ModifiedAt == nil || *numeric.ModifiedAt != "1788600000" {
		t.Fatalf("numeric mtime = %v err = %v", numeric.ModifiedAt, err)
	}
	text, err := normalizeItem(t, `{"id":1,"fname":"a","mtime":"2026-09-05"}`)
	if err != nil || text.ModifiedAt == nil || *text.ModifiedAt != "2026-09-05" {
		t.Fatalf("string mtime = %v err = %v", text.ModifiedAt, err)
	}
}

func TestEntryFromItemEtagNormalization(t *testing.T) {
	tests := []struct {
		name string
		item string
		want *string
	}{
		{"missing", `{"id":1,"fname":"a"}`, nil},
		{"null", `{"id":1,"fname":"a","fsha":null}`, nil},
		{"non string", `{"id":1,"fname":"a","fsha":5}`, nil},
		{"control char", `{"id":1,"fname":"a","fsha":"abc\u0001def"}`, nil},
		{"too long", `{"id":1,"fname":"a","fsha":"` + strings.Repeat("a", 4097) + `"}`, nil},
	}
	for _, test := range tests {
		entry, err := normalizeItem(t, test.item)
		if err != nil {
			t.Fatalf("%s failed: %v", test.name, err)
		}
		if test.want == nil && entry.Etag != nil {
			t.Fatalf("%s etag = %v, want nil", test.name, entry.Etag)
		}
	}
	kept, err := normalizeItem(t, `{"id":1,"fname":"a","fsha":"`+strings.Repeat("a", 4096)+`"}`)
	if err != nil || kept.Etag == nil || len(*kept.Etag) != 4096 {
		t.Fatalf("boundary etag = %v err = %v", kept.Etag, err)
	}
}

func TestEntryFromItemParentAndLinkNormalization(t *testing.T) {
	tests := []struct {
		name      string
		item      string
		wantParet *string
		wantLink  *string
	}{
		{"missing both", `{"id":1,"fname":"a"}`, nil, nil},
		{"null parent", `{"id":1,"fname":"a","parentid":null}`, nil, nil},
		{"numeric parent", `{"id":1,"fname":"a","parentid":3}`, strPtr("3"), nil},
		{"boolean parent", `{"id":1,"fname":"a","parentid":true}`, strPtr("True"), nil},
		{"empty link", `{"id":1,"fname":"a","link_id":""}`, nil, nil},
		{"zero link", `{"id":1,"fname":"a","link_id":0}`, nil, nil},
		{"false link", `{"id":1,"fname":"a","link_id":false}`, nil, nil},
		{"null link", `{"id":1,"fname":"a","link_id":null}`, nil, nil},
		{"real link", `{"id":1,"fname":"a","link_id":"cid-1"}`, nil, strPtr("cid-1")},
		{"numeric link", `{"id":1,"fname":"a","link_id":42}`, nil, strPtr("42")},
	}
	for _, test := range tests {
		entry, err := normalizeItem(t, test.item)
		if err != nil {
			t.Fatalf("%s failed: %v", test.name, err)
		}
		if (entry.ParentID == nil) != (test.wantParet == nil) ||
			(entry.ParentID != nil && *entry.ParentID != *test.wantParet) {
			t.Fatalf("%s parent = %v, want %v", test.name, entry.ParentID, test.wantParet)
		}
		if (entry.LinkID == nil) != (test.wantLink == nil) ||
			(entry.LinkID != nil && *entry.LinkID != *test.wantLink) {
			t.Fatalf("%s link = %v, want %v", test.name, entry.LinkID, test.wantLink)
		}
	}
}

func TestPyTruthy(t *testing.T) {
	tests := []struct {
		value any
		want  bool
	}{
		{nil, false},
		{false, false},
		{true, true},
		{"", false},
		{"x", true},
		{json.Number("0"), false},
		{json.Number("0.0"), false},
		{json.Number("1"), true},
		{[]any{}, false},
		{[]any{nil}, true},
		{map[string]any{}, false},
		{map[string]any{"a": nil}, true},
	}
	for _, test := range tests {
		if got := pyTruthy(test.value); got != test.want {
			t.Fatalf("pyTruthy(%v) = %v, want %v", test.value, got, test.want)
		}
	}
}

func strPtr(value string) *string {
	return model.Ptr(value)
}
