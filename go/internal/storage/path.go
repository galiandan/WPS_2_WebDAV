// Package storage resolves human-readable business paths against WPS file
// IDs and exposes the confirmed drive operations on top of the wps client.
//
// Path handling follows the D-04 decision: every HTTP entry decodes its URL
// exactly once (net/url request-line parsing for DAV paths, form-style query
// parsing for REST query values), and the functions in this package treat
// their input as that already-decoded business path. Nothing here
// percent-decodes a second time, so a segment containing '%' keeps it
// literally; handlers must therefore feed these functions r.URL.Path or the
// parsed query value directly and must never route requests through ServeMux,
// whose automatic path cleaning would rewrite business paths.
package storage

import (
	"strings"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// MaxRemoteNameBytes mirrors storage.py's MAX_REMOTE_NAME_BYTES: one remote
// name may not exceed this many UTF-8 bytes.
const MaxRemoteNameBytes = 4096

// SplitRemotePath validates an absolute, already-URL-decoded business path
// and returns its remote name components in order. The root path yields no
// components. Exactly one transport decode has happened before this call
// (D-04), so no percent-decoding happens here.
func SplitRemotePath(path string) ([]string, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, model.NewStorageError(model.KindInvalidPath, "remote paths must start with '/'")
	}
	if !utf8.ValidString(path) {
		return nil, model.NewStorageError(model.KindInvalidPath, "remote path is not valid UTF-8")
	}
	if containsForbiddenChar(path) {
		return nil, model.NewStorageError(model.KindInvalidPath, "remote path contains a forbidden character")
	}
	if path == "/" {
		return []string{}, nil
	}

	rawParts := strings.Split(path, "/")[1:]
	if len(rawParts) > 0 && rawParts[len(rawParts)-1] == "" {
		rawParts = rawParts[:len(rawParts)-1]
	}
	for _, part := range rawParts {
		if part == "" || part == "." || part == ".." {
			return nil, model.NewStorageError(model.KindInvalidPath, "remote path contains an empty or traversal component")
		}
	}
	for _, part := range rawParts {
		if strings.Contains(part, "\x00") || strings.Contains(part, "/") || len(part) > MaxRemoteNameBytes {
			return nil, model.NewStorageError(model.KindInvalidPath, "remote path contains a forbidden component")
		}
	}
	return rawParts, nil
}

// containsForbiddenChar mirrors the whole-path scan in split_remote_path:
// backslashes, NUL, C0 control bytes, and DEL are rejected wherever they
// appear. Non-ASCII codepoints are never forbidden, which a byte scan
// preserves because UTF-8 continuation bytes are all >= 0x80.
func containsForbiddenChar(path string) bool {
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '\\' || c < 0x20 || c == 0x7F {
			return true
		}
	}
	return false
}

// JoinRemotePath builds the canonical business path from already validated
// remote names. The trailingSlash argument mirrors storage.py's
// join_remote_path keyword: its posixpath.normpath call always removes a
// trailing slash, so both values return the same canonical path. The
// argument survives for call-site parity with the Python reference and the
// behaviour is pinned by a golden test rather than silently dropped.
func JoinRemotePath(parts []string, trailingSlash bool) (string, error) {
	for _, part := range parts {
		if part == "" || part == "." || part == ".." ||
			strings.Contains(part, "/") || containsForbiddenChar(part) ||
			len(part) > MaxRemoteNameBytes {
			return "", model.NewStorageError(model.KindInvalidPath, "remote path contains an invalid component")
		}
	}
	path := "/" + strings.Join(parts, "/")
	if trailingSlash && path != "/" {
		path += "/"
	}
	if path != "/" {
		// posixpath.normpath on an already-validated join only ever strips
		// the trailing slash added just above; separators and traversal are
		// impossible in validated parts.
		path = strings.TrimSuffix(path, "/")
	}
	return path, nil
}

// QuoteRemoteSegment percent-encodes one remote name for use as a URL path
// segment, mirroring urllib.parse.quote(name, safe=""): unreserved ASCII
// (A-Z a-z 0-9 - _ . ~) passes through and every other byte becomes %XX on
// the UTF-8 form. This is deliberately stricter than Go's url.PathEscape,
// which would leave reserved characters such as '+' '@' and '/' unescaped.
func QuoteRemoteSegment(name string) string {
	var b strings.Builder
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xF])
		}
	}
	return b.String()
}

// EncodedPath renders validated remote names as a percent-encoded URL path
// with a leading slash, mirroring the DAV href builder's
// "/".join(quote(part, safe="")). Each segment is encoded independently, so
// a slash inside a name can never merge two segments; empty parts render
// the root "/". A DAV prefix is prepended by the HTTP layer.
func EncodedPath(parts []string) string {
	encoded := make([]string, len(parts))
	for i, part := range parts {
		encoded[i] = QuoteRemoteSegment(part)
	}
	return "/" + strings.Join(encoded, "/")
}
