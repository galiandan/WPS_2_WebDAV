package httpserver

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// assetRoot is the fixed static-asset prefix. Like the health path it is
// never configurable (Python hardcodes it in _web_asset_name).
const assetRoot = "/assets/"

// SplitRequestTarget mirrors urllib.parse.urlsplit(...).path and .query for
// the request-target forms an HTTP server can receive: it strips an optional
// scheme and authority ("http://host/dav/x" and the protocol-relative
// "//host/dav/x" both reduce to "/dav/x"), then the fragment, then splits
// off the query. The returned path stays percent-encoded; only the DAV
// extraction decodes it (D-04). Python's tab/CR/LF stripping is preserved
// even though Go's transport usually rejects such request lines earlier.
func SplitRequestTarget(target string) (path, rawQuery string) {
	target = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(
		target, "\t", ""), "\r", ""), "\n", "")
	if schemeEnd := urlSchemeEnd(target); schemeEnd > 0 {
		target = target[schemeEnd+1:]
	}
	if strings.HasPrefix(target, "//") {
		// Python treats a leading "//" as authority, not path: the netloc
		// ends at the next "/", "?" or "#".
		rest := target[2:]
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			target = rest[i:]
		} else {
			target = ""
		}
	}
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target = target[:i]
	}
	if i := strings.IndexByte(target, '?'); i >= 0 {
		return target[:i], target[i+1:]
	}
	return target, ""
}

// urlSchemeEnd mirrors Python's scheme detection: a colon at index i > 0
// whose prefix consists only of scheme characters starting with an ASCII
// letter. Anything else is part of the path.
func urlSchemeEnd(target string) int {
	i := strings.IndexByte(target, ':')
	if i <= 0 {
		return -1
	}
	for j := 0; j < i; j++ {
		c := target[j]
		alpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if j == 0 {
			if !alpha {
				return -1
			}
			continue
		}
		if !alpha && !(c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.') {
			return -1
		}
	}
	return i
}

// parseQueryValues mirrors urllib.parse.parse_qs(..., keep_blank_values=True):
// pairs split on "&", empty pairs skipped, name and value split on the first
// "=", a missing value becomes "", and each part decodes with unquote_plus
// semantics — "+" means space, malformed escapes stay literal, and invalid
// UTF-8 bytes are replaced per maximal subpart like CPython's
// bytes.decode("utf-8", "replace").
func parseQueryValues(rawQuery string) url.Values {
	values := url.Values{}
	if rawQuery == "" {
		return values
	}
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		name, value, _ := strings.Cut(pair, "=")
		values.Add(decodeQueryComponent(name), decodeQueryComponent(value))
	}
	return values
}

func decodeQueryComponent(component string) string {
	decoded := unquotePercent(strings.ReplaceAll(component, "+", " "))
	if utf8.ValidString(decoded) {
		return decoded
	}
	return decodeUTF8Replace(decoded)
}

// unquotePercent decodes valid %XX escapes to raw bytes and leaves malformed
// escapes literal, mirroring urllib.parse.unquote. Unlike url.PathUnescape
// it never fails and never validates UTF-8; storage rejects invalid UTF-8
// with Python's exact message, and the query parser replaces it.
func unquotePercent(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '%' && i+3 <= len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]) {
			out = append(out, unhexDigit(s[i+1])<<4|unhexDigit(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhexDigit(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// decodeUTF8Replace mirrors CPython's bytes.decode("utf-8", "replace"):
// one U+FFFD per maximal invalid subpart, resuming at the first offending
// byte. strings.ToValidUTF8 differs (one replacement per run), so the
// subpart table is implemented directly.
func decodeUTF8Replace(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		// A properly encoded U+FFFD also decodes to RuneError; only
		// size == 1 means the bytes are invalid.
		if r != utf8.RuneError || size != 1 {
			out.WriteString(s[i : i+size])
			i += size
			continue
		}
		out.WriteRune(utf8.RuneError)
		i += invalidSubpartLen(s[i:])
	}
	return out.String()
}

// invalidSubpartLen reports how many bytes CPython consumes for one
// replacement at the start of s: the start byte plus the continuation bytes
// its shape allows, following the Unicode maximal-subpart recommendation.
func invalidSubpartLen(s string) int {
	if len(s) == 0 {
		return 0
	}
	c := s[0]
	var want int
	start, end := byte(0x80), byte(0xBF)
	switch {
	case c >= 0x80 && c < 0xC2:
		want = 1 // stray continuation byte or overlong start
	case c < 0xE0:
		want, start, end = 2, 0x80, 0xBF
	case c < 0xF0:
		want, start, end = 3, 0x80, 0xBF
		if c == 0xE0 {
			start = 0xA0
		} else if c == 0xED {
			end = 0x9F
		}
	case c < 0xF5:
		want, start, end = 4, 0x80, 0xBF
		if c == 0xF0 {
			start = 0x90
		} else if c == 0xF4 {
			end = 0x8F
		}
	default:
		want = 1 // F5..FF can never start a sequence
	}
	n := 1
	for n < want && n < len(s) && s[n] >= start && s[n] <= end {
		n++
	}
	return n
}
