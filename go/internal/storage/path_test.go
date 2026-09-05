package storage

import (
	"net/url"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// Golden inputs for SplitRemotePath. The wire column documents the raw HTTP
// form whose single transport decode produced the business path, pinning the
// D-04 decision that business segments are decoded exactly once and keep a
// literal '%'.
func TestSplitRemotePathGolden(t *testing.T) {
	cases := []struct {
		name string
		wire string
		path string
		want []string
	}{
		{name: "root", wire: "/", path: "/", want: []string{}},
		{name: "single", wire: "/a", path: "/a", want: []string{"a"}},
		{name: "nested", wire: "/a/b/c", path: "/a/b/c", want: []string{"a", "b", "c"}},
		{name: "trailing slash tolerated", wire: "/a/b/", path: "/a/b/", want: []string{"a", "b"}},
		{
			name: "encoded slash stays a name (D-04)",
			// /weird%252Fname.txt decodes once to the business path below;
			// Python's second unquote turned it into "/weird/name.txt" and
			// missed. The Go business layer must keep "%2F" literally.
			wire: "/weird%252Fname.txt",
			path: "/weird%2Fname.txt",
			want: []string{"weird%2Fname.txt"},
		},
		{
			name: "double-encoded slash survives one decode",
			// %25252F decodes once to %252F and must not decode again.
			wire: "/weird%25252Fname.txt",
			path: "/weird%252Fname.txt",
			want: []string{"weird%252Fname.txt"},
		},
		{name: "percent pair stays literal", wire: "/a%2525b", path: "/a%25b", want: []string{"a%25b"}},
		{name: "lone percent stays literal", wire: "/100%25.txt", path: "/100%.txt", want: []string{"100%.txt"}},
		{name: "malformed escape never decoded here", wire: "(unreachable via HTTP: transport rejects)", path: "/a%ZZb", want: []string{"a%ZZb"}},
		{
			name: "plus stays plus on DAV paths",
			// REST query parsing turns '+' into a space before this layer;
			// a DAV path keeps it literally.
			wire: "/a+b",
			path: "/a+b",
			want: []string{"a+b"},
		},
		{name: "space stays", wire: "/a%20b", path: "/a b", want: []string{"a b"}},
		{name: "chinese", wire: "/%E4%B8%AD%E6%96%87/%E7%9B%AE%E5%BD%95", path: "/中文/目录", want: []string{"中文", "目录"}},
		{name: "emoji", wire: "/%F0%9F%98%80/%F0%9F%93%81", path: "/😀/📁", want: []string{"😀", "📁"}},
		{name: "mixed punctuation", wire: "/Report+2026%20final%2Bv2%40x.txt", path: "/Report 2026 final+v2@x.txt", want: []string{"Report 2026 final+v2@x.txt"}},
		{name: "name at byte limit", wire: "(4096 ASCII bytes)", path: "/" + strings.Repeat("a", MaxRemoteNameBytes), want: []string{strings.Repeat("a", MaxRemoteNameBytes)}},
		{name: "multibyte name at byte limit", wire: "(4096 UTF-8 bytes)", path: "/" + strings.Repeat("中", 1365) + "a", want: []string{strings.Repeat("中", 1365) + "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitRemotePath(tc.path)
			if err != nil {
				t.Fatalf("SplitRemotePath(%q) error: %v", tc.path, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("SplitRemotePath(%q) = %q, want %q", tc.path, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("SplitRemotePath(%q) = %q, want %q", tc.path, got, tc.want)
				}
			}
		})
	}
}

func TestSplitRemotePathRejects(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		message string
	}{
		{name: "empty", path: "", message: "remote paths must start with '/'"},
		{name: "relative", path: "a/b", message: "remote paths must start with '/'"},
		{name: "bare name", path: "a", message: "remote paths must start with '/'"},
		{name: "invalid utf-8", path: "/a\xffb", message: "remote path is not valid UTF-8"},
		{name: "invalid utf-8 before forbidden char", path: "/\xff\\x", message: "remote path is not valid UTF-8"},
		{name: "raw ff byte from transport decode", path: "/%\xff", message: "remote path is not valid UTF-8"},
		{name: "backslash", path: `/a\b`, message: "remote path contains a forbidden character"},
		{name: "nul", path: "/a\x00b", message: "remote path contains a forbidden character"},
		{name: "tab", path: "/a\tb", message: "remote path contains a forbidden character"},
		{name: "newline", path: "/a\nb", message: "remote path contains a forbidden character"},
		{name: "del", path: "/a\x7fb", message: "remote path contains a forbidden character"},
		{name: "empty middle segment", path: "/a//b", message: "remote path contains an empty or traversal component"},
		{name: "double slash root", path: "//a", message: "remote path contains an empty or traversal component"},
		{name: "only double slash", path: "//", message: "remote path contains an empty or traversal component"},
		{name: "dot", path: "/.", message: "remote path contains an empty or traversal component"},
		{name: "dot dot", path: "/..", message: "remote path contains an empty or traversal component"},
		{name: "dot middle", path: "/a/./b", message: "remote path contains an empty or traversal component"},
		{name: "dot dot middle", path: "/a/../b", message: "remote path contains an empty or traversal component"},
		{name: "trailing dot", path: "/a/.", message: "remote path contains an empty or traversal component"},
		{name: "two trailing slashes", path: "/a/b//", message: "remote path contains an empty or traversal component"},
		{name: "traversal reported before length", path: "/a/../" + strings.Repeat("a", MaxRemoteNameBytes+1), message: "remote path contains an empty or traversal component"},
		{name: "overlong ascii segment", path: "/" + strings.Repeat("a", MaxRemoteNameBytes+1), message: "remote path contains a forbidden component"},
		{name: "overlong multibyte segment", path: "/" + strings.Repeat("中", 1366), message: "remote path contains a forbidden component"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitRemotePath(tc.path)
			if err == nil {
				t.Fatalf("SplitRemotePath(%q) = %q, want error", tc.path, got)
			}
			if err.Error() != tc.message {
				t.Fatalf("SplitRemotePath(%q) error = %q, want %q", tc.path, err.Error(), tc.message)
			}
			storageErr, ok := model.AsStorageError(err)
			if !ok {
				t.Fatalf("SplitRemotePath(%q) error %T is not a *model.StorageError", tc.path, err)
			}
			if storageErr.Kind != model.KindInvalidPath {
				t.Fatalf("SplitRemotePath(%q) kind = %q, want invalid_path", tc.path, storageErr.Kind)
			}
		})
	}
}

func TestJoinRemotePathGolden(t *testing.T) {
	cases := []struct {
		name          string
		parts         []string
		trailingSlash bool
		want          string
	}{
		{name: "root", parts: []string{}, want: "/"},
		{name: "root with flag", parts: []string{}, trailingSlash: true, want: "/"},
		{name: "single", parts: []string{"a"}, want: "/a"},
		{name: "nested", parts: []string{"a", "b"}, want: "/a/b"},
		{
			name:          "trailing slash flag is neutralized like posixpath.normpath",
			parts:         []string{"a", "b"},
			trailingSlash: true,
			want:          "/a/b",
		},
		{name: "percent segment", parts: []string{"a%2Fb"}, want: "/a%2Fb"},
		{name: "chinese", parts: []string{"中文", "目录"}, want: "/中文/目录"},
		{name: "ellipsis is not traversal", parts: []string{"..."}, want: "/..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JoinRemotePath(tc.parts, tc.trailingSlash)
			if err != nil {
				t.Fatalf("JoinRemotePath(%q) error: %v", tc.parts, err)
			}
			if got != tc.want {
				t.Fatalf("JoinRemotePath(%q) = %q, want %q", tc.parts, got, tc.want)
			}
		})
	}
}

func TestJoinRemotePathRejects(t *testing.T) {
	cases := []struct {
		name  string
		parts []string
	}{
		{name: "empty part", parts: []string{""}},
		{name: "dot", parts: []string{"."}},
		{name: "dot dot", parts: []string{".."}},
		{name: "slash inside", parts: []string{"a/b"}},
		{name: "backslash inside", parts: []string{`a\b`}},
		{name: "newline", parts: []string{"a\nb"}},
		{name: "nul", parts: []string{"a\x00b"}},
		{name: "del", parts: []string{"a\x7fb"}},
		{name: "overlong", parts: []string{strings.Repeat("a", MaxRemoteNameBytes+1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JoinRemotePath(tc.parts, false)
			if err == nil {
				t.Fatalf("JoinRemotePath(%q) = %q, want error", tc.parts, got)
			}
			if err.Error() != "remote path contains an invalid component" {
				t.Fatalf("JoinRemotePath(%q) error = %q", tc.parts, err.Error())
			}
		})
	}
}

func TestJoinRoundTripsSplit(t *testing.T) {
	for _, path := range []string{"/", "/a", "/a/b/c", "/a b/c+d", "/中文/目录/😀", "/weird%2Fname.txt/100%.txt"} {
		parts, err := SplitRemotePath(path)
		if err != nil {
			t.Fatalf("SplitRemotePath(%q) error: %v", path, err)
		}
		joined, err := JoinRemotePath(parts, false)
		if err != nil {
			t.Fatalf("JoinRemotePath(%q) error: %v", parts, err)
		}
		if joined != path {
			t.Fatalf("round trip of %q = %q", path, joined)
		}
	}
}

func TestQuoteRemoteSegmentGolden(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "unreserved ascii", input: "name-._~1", want: "name-._~1"},
		{name: "space", input: "a b", want: "a%20b"},
		{name: "plus is encoded", input: "a+b", want: "a%2Bb"},
		{name: "slash is encoded", input: "a/b", want: "a%2Fb"},
		{name: "percent is encoded", input: "100%.txt", want: "100%25.txt"},
		{name: "double encoded name", input: "weird%2Fname.txt", want: "weird%252Fname.txt"},
		{name: "chinese", input: "中文", want: "%E4%B8%AD%E6%96%87"},
		{name: "emoji", input: "😀", want: "%F0%9F%98%80"},
		{
			name:  "reserved characters beyond url.PathEscape",
			input: "a@b&c=d,e;f$g:h?i/j",
			want:  "a%40b%26c%3Dd%2Ce%3Bf%24g%3Ah%3Fi%2Fj",
		},
		{name: "tab", input: "\t", want: "%09"},
		{name: "nul", input: "\x00", want: "%00"},
		{name: "del", input: "\x7f", want: "%7F"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QuoteRemoteSegment(tc.input); got != tc.want {
				t.Fatalf("QuoteRemoteSegment(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestQuoteRemoteSegmentRoundTripsThroughOneDecode(t *testing.T) {
	for _, name := range []string{"a b", "a+b", "a/b", "100%.txt", "weird%2Fname.txt", "中文", "😀", "name-._~1", "\t\x7f"} {
		encoded := QuoteRemoteSegment(name)
		decoded, err := url.PathUnescape(encoded)
		if err != nil {
			t.Fatalf("PathUnescape(%q) error: %v", encoded, err)
		}
		if decoded != name {
			t.Fatalf("one decode of %q = %q, want %q", encoded, decoded, name)
		}
	}
}

func TestEncodedPathGolden(t *testing.T) {
	cases := []struct {
		name  string
		parts []string
		want  string
	}{
		{name: "root", parts: []string{}, want: "/"},
		{name: "segments encoded separately", parts: []string{"a b", "c"}, want: "/a%20b/c"},
		{name: "slash in name cannot merge segments", parts: []string{"weird%2Fname.txt"}, want: "/weird%252Fname.txt"},
		{name: "unicode", parts: []string{"中文", "😀"}, want: "/%E4%B8%AD%E6%96%87/%F0%9F%98%80"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EncodedPath(tc.parts); got != tc.want {
				t.Fatalf("EncodedPath(%q) = %q, want %q", tc.parts, got, tc.want)
			}
		})
	}
}

// TestTransportDecodeContract pins the exact stdlib primitives the HTTP
// stage must use for its single decode (D-04): request paths via
// url.PathUnescape semantics, REST query values via form-style parsing where
// '+' means space, and neither decoding recursively.
func TestTransportDecodeContract(t *testing.T) {
	t.Run("request path decodes once and keeps plus", func(t *testing.T) {
		decoded, err := url.PathUnescape("/weird%252Fname.txt")
		if err != nil {
			t.Fatal(err)
		}
		if decoded != "/weird%2Fname.txt" {
			t.Fatalf("PathUnescape = %q", decoded)
		}
		parts, err := SplitRemotePath(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) != 1 || parts[0] != "weird%2Fname.txt" {
			t.Fatalf("business parts = %q", parts)
		}
		if plus, err := url.PathUnescape("/a+b"); err != nil || plus != "/a+b" {
			t.Fatalf("PathUnescape(/a+b) = %q, %v", plus, err)
		}
	})

	t.Run("query decodes once and maps plus to space", func(t *testing.T) {
		values, err := url.ParseQuery("path=%2Fweird%252Fname.txt")
		if err != nil {
			t.Fatal(err)
		}
		parts, err := SplitRemotePath(values.Get("path"))
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) != 1 || parts[0] != "weird%2Fname.txt" {
			t.Fatalf("business parts = %q", parts)
		}
		values, err = url.ParseQuery("path=%2Fa+b")
		if err != nil {
			t.Fatal(err)
		}
		if values.Get("path") != "/a b" {
			t.Fatalf("query path = %q, want /a b", values.Get("path"))
		}
	})

	t.Run("single decode never recurses", func(t *testing.T) {
		decoded, err := url.PathUnescape("%25252F")
		if err != nil {
			t.Fatal(err)
		}
		if decoded != "%252F" {
			t.Fatalf("PathUnescape(%%25252F) = %q, want %%252F", decoded)
		}
	})

	t.Run("invalid utf-8 from one decode is rejected", func(t *testing.T) {
		decoded, err := url.PathUnescape("%FF")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SplitRemotePath("/a" + decoded); err == nil || err.Error() != "remote path is not valid UTF-8" {
			t.Fatalf("SplitRemotePath error = %v", err)
		}
		values, err := url.ParseQuery("path=%2F%FF")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SplitRemotePath(values.Get("path")); err == nil || err.Error() != "remote path is not valid UTF-8" {
			t.Fatalf("SplitRemotePath error = %v", err)
		}
	})

	t.Run("request-line parse decodes into URL.Path once", func(t *testing.T) {
		parsed, err := url.ParseRequestURI("/dav/weird%252Fname.txt")
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Path != "/dav/weird%2Fname.txt" {
			t.Fatalf("Path = %q, want /dav/weird%%2Fname.txt", parsed.Path)
		}
		// A raw %2F decodes into an indistinguishable real slash: under the
		// one-decode contract the business path simply splits there.
		parsed, err = url.ParseRequestURI("/dav/weird%2Fname.txt")
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Path != "/dav/weird/name.txt" {
			t.Fatalf("Path = %q, want /dav/weird/name.txt", parsed.Path)
		}
	})
}
