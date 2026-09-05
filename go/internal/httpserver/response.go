package httpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	contentTypeText = "text/plain; charset=utf-8"
	contentTypeJSON = "application/json; charset=utf-8"
	contentTypeHTML = "text/html;charset=utf-8"
)

// writeResponse mirrors Python's _send_bytes: fixed Content-Type and
// Content-Length, Cache-Control no-store, then the caller's extra headers,
// then Connection: close when the connection is marked for closing and the
// extra headers do not already carry one. HEAD requests receive the headers
// but no body.
func writeResponse(w http.ResponseWriter, r *http.Request, status int, body []byte, contentType string, extra map[string]string, closeConn bool) {
	header := w.Header()
	header.Set("Content-Type", contentType)
	header.Set("Content-Length", strconv.Itoa(len(body)))
	header.Set("Cache-Control", "no-store")
	for name, value := range extra {
		header.Set(name, value)
	}
	if closeConn && !hasHeader(extra, "Connection") {
		header.Set("Connection", "close")
	}
	w.WriteHeader(status)
	if r.Method != "HEAD" && len(body) > 0 {
		w.Write(body)
	}
}

func hasHeader(extra map[string]string, name string) bool {
	for key := range extra {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// sendError mirrors Python's _send_error: REST callers receive compact JSON
// {"error": message}; every other route receives the message plus a single
// trailing newline as text/plain.
func sendError(w http.ResponseWriter, r *http.Request, status int, message string, rest bool, extra map[string]string, closeConn bool) {
	if !rest {
		writeResponse(w, r, status, []byte(message+"\n"), contentTypeText, extra, closeConn)
		return
	}
	payload, err := marshalPythonJSON(map[string]string{"error": message})
	if err != nil {
		payload = []byte(`{"error":"internal server error"}`)
	}
	writeResponse(w, r, status, payload, contentTypeJSON, extra, closeConn)
}

// marshalPythonJSON renders v the way Python's
// json.dumps(payload, ensure_ascii=True, separators=(",", ":")) does: one
// compact line, HTML characters left raw, and every rune outside printable
// ASCII emitted as \uXXXX. The REST error payloads must stay byte-identical
// to the Python adapter's for the contract evidence to keep comparing.
func marshalPythonJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return asciiEscape(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// asciiEscape rewrites Go's compact JSON into Python's ensure_ascii form.
// Structural bytes and escape sequences are all printable ASCII, so the
// transform can walk raw bytes without tokenizing the JSON.
func asciiEscape(in []byte) []byte {
	plain := true
	for _, b := range in {
		if b < 0x20 || b > 0x7E {
			plain = false
			break
		}
	}
	if plain {
		return in
	}
	var out bytes.Buffer
	out.Grow(len(in) + 16)
	for i := 0; i < len(in); {
		b := in[i]
		if b < utf8.RuneSelf {
			if b < 0x20 || b == 0x7F {
				fmt.Fprintf(&out, `\u%04x`, b)
			} else {
				out.WriteByte(b)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(in[i:])
		if r == utf8.RuneError && size == 1 {
			// The JSON encoder never emits invalid UTF-8; pass it through
			// rather than corrupting the payload.
			out.WriteByte(b)
			i++
			continue
		}
		writeUnicodeEscape(&out, r)
		i += size
	}
	return out.Bytes()
}

func writeUnicodeEscape(out *bytes.Buffer, r rune) {
	if r > 0xFFFF {
		high, low := utf16.EncodeRune(r)
		fmt.Fprintf(out, `\u%04x\u%04x`, high, low)
		return
	}
	fmt.Fprintf(out, `\u%04x`, r)
}
