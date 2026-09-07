package httpserver

import (
	"net/url"
	"testing"
)

// TestSplitRequestTarget pins the urlsplit mirror against behaviors probed
// from the Python reference (urllib.parse.urlsplit(self.path).
func TestSplitRequestTarget(t *testing.T) {
	cases := []struct {
		target string
		path   string
		query  string
	}{
		{"/dav/x", "/dav/x", ""},
		{"/dav/x?y", "/dav/x", "y"},
		{"/dav/x#y", "/dav/x", ""},
		{"/a?b#c", "/a", "b"},
		{"/a#b?c", "/a", ""},
		{"*", "*", ""},
		{"http://h/dav/x", "/dav/x", ""},
		{"http://host", "", ""},
		{"https://h:8443/api/v1/status?x=1", "/api/v1/status", "x=1"},
		{"//host/api/v1/status", "/api/v1/status", ""},
		{"//dav/x", "/x", ""},
		{"foo:bar/dav", "bar/dav", ""},
		{"/x://y", "/x://y", ""},
		{"/healthz?x=1", "/healthz", "x=1"},
		{"", "", ""},
	}
	for _, tc := range cases {
		path, query := SplitRequestTarget(tc.target)
		if path != tc.path || query != tc.query {
			t.Errorf("SplitRequestTarget(%q) = (%q, %q), want (%q, %q)",
				tc.target, path, query, tc.path, tc.query)
		}
	}
}

// TestSplitRequestTargetStripsTabs pins the URL byte-stripping Python applies
// before parsing (_UNSAFE_URL_BYTES_TO_REMOVE).
func TestSplitRequestTargetStripsTabs(t *testing.T) {
	path, _ := SplitRequestTarget("/da\tv/x")
	if path != "/dav/x" {
		t.Fatalf("path with tab = %q, want /dav/x", path)
	}
}

// TestUnquotePercent pins the DAV business-path decode: one pass, malformed
// escapes literal, invalid UTF-8 preserved for storage to reject.
func TestUnquotePercent(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/a b", "/a b"},
		{"/a%20b", "/a b"},
		{"/a%2Fb", "/a/b"},
		{"/a%252Fb", "/a%2Fb"},
		{"/a%2G", "/a%2G"},
		{"/a%2", "/a%2"},
		{"/a%FF", "/a\xff"},
		{"%C3%A9", "é"},
		{"a+b", "a+b"},
	}
	for _, tc := range cases {
		if got := unquotePercent(tc.in); got != tc.want {
			t.Errorf("unquotePercent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseQueryValues pins parse_qs(keep_blank_values=True) semantics,
// probed from the Python reference.
func TestParseQueryValues(t *testing.T) {
	cases := []struct {
		raw  string
		want url.Values
	}{
		{"", url.Values{}},
		{"path=%2F", url.Values{"path": {"/"}}},
		{"path=%2Fa+b", url.Values{"path": {"/a b"}}},
		{"path=%2Fweird%252Fname.txt", url.Values{"path": {"/weird%2Fname.txt"}}},
		{"p=a=b=c", url.Values{"p": {"a=b=c"}}},
		{"a", url.Values{"a": {""}}},
		{"a=", url.Values{"a": {""}}},
		{"=v", url.Values{"": {"v"}}},
		{"a=1&&b=2", url.Values{"a": {"1"}, "b": {"2"}}},
		{"p=%2G", url.Values{"p": {"%2G"}}},
		{"p=%FF", url.Values{"p": {"\ufffd"}}},
		{"p=%FF%FE", url.Values{"p": {"\ufffd\ufffd"}}},
		{"p=%C3%28", url.Values{"p": {"\ufffd("}}},
		{"p=%2F%C3", url.Values{"p": {"/\ufffd"}}},
		{"p=%ED%A0%80", url.Values{"p": {"\ufffd\ufffd\ufffd"}}},
		{"p=%F4%90%80%80", url.Values{"p": {"\ufffd\ufffd\ufffd\ufffd"}}},
		{"p=%F0%9F%98%80", url.Values{"p": {"😀"}}},
		{"p=%F1%80%80%80", url.Values{"p": {"\U00040000"}}},
		{"p=%2B", url.Values{"p": {"+"}}},
		{"a=1&a=2", url.Values{"a": {"1", "2"}}},
	}
	for _, tc := range cases {
		got := parseQueryValues(tc.raw)
		if len(got) != len(tc.want) {
			t.Errorf("parseQueryValues(%q) = %v, want %v", tc.raw, got, tc.want)
			continue
		}
		for key, wantValues := range tc.want {
			gotValues, ok := got[key]
			if !ok || len(gotValues) != len(wantValues) {
				t.Errorf("parseQueryValues(%q)[%q] = %v, want %v", tc.raw, key, gotValues, wantValues)
				continue
			}
			for i := range wantValues {
				if gotValues[i] != wantValues[i] {
					t.Errorf("parseQueryValues(%q)[%q][%d] = %q, want %q", tc.raw, key, i, gotValues[i], wantValues[i])
				}
			}
		}
	}
}
