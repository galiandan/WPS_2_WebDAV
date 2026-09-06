package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
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
		// Raw map assignment keeps the caller's exact spelling on the
		// wire (Set would canonicalize "DAV" to "Dav", which Python
		// never does); every existing caller passes canonical keys.
		header[name] = []string{value}
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

// ControlLimits mirrors AdapterApplication's response and body caps. The
// app assembly wires them from configuration; zero fields fall back to the
// Python defaults.
type ControlLimits struct {
	MaxControlBody  int64
	MaxResponseBody int64
}

// DefaultControlLimits mirrors the AdapterApplication defaults: 1 MiB
// control bodies, 16 MiB control responses.
func DefaultControlLimits() ControlLimits {
	return ControlLimits{
		MaxControlBody:  1024 * 1024,
		MaxResponseBody: 16 * 1024 * 1024,
	}
}

// sendJSON mirrors Python's _send_json: compact ensure_ascii JSON with the
// response size limit enforced before any header is written. An oversized
// response becomes a KindInsufficientStorage error which mapError turns
// into 507, exactly like the Python raise inside _send_json.
func sendJSON(w http.ResponseWriter, r *http.Request, status int, payload any, limits ControlLimits, extra map[string]string) error {
	if limits.MaxResponseBody <= 0 {
		limits = DefaultControlLimits()
	}
	body, err := marshalPythonJSON(payload)
	if err != nil {
		return err
	}
	if int64(len(body)) > limits.MaxResponseBody {
		return model.NewStorageError(model.KindInsufficientStorage, "response exceeds the configured size limit")
	}
	writeResponse(w, r, status, body, contentTypeJSON, extra, false)
	return nil
}

// requestBodyTooLarge mirrors Python's _RequestBodyTooLarge: an empty
// message and a closed connection on top of the 413.
type requestBodyTooLarge struct{}

func (requestBodyTooLarge) Error() string { return "" }

func errRequestBodyTooLarge() error { return requestBodyTooLarge{} }

// controlRequestError carries the 400-class protocol errors Python raises
// as bare ValueError/TypeError in its request-reading helpers (invalid
// JSON bodies, query parameter misuse, ...). Body framing failures may
// also close the connection like Python's close_connection assignments.
type controlRequestError struct {
	message   string
	closeConn bool
}

func (e *controlRequestError) Error() string { return e.message }

func errBadRequest(message string) error { return &controlRequestError{message: message} }
func errBadRequestClose(message string) error {
	return &controlRequestError{message: message, closeConn: true}
}

// mapError mirrors Python's _handle_exception: the complete domain error
// status table, with the rest context choosing compact JSON or text
// framing. Upstream WPS failures are reduced to fixed redacted codes —
// response bodies, URLs, and signed object details never reach the client.
func mapError(w http.ResponseWriter, r *http.Request, err error, rest bool) {
	var tooLarge requestBodyTooLarge
	if errors.As(err, &tooLarge) {
		// Python raises _RequestBodyTooLarge without a message, so both
		// framings carry an empty message over a closed connection.
		sendError(w, r, http.StatusRequestEntityTooLarge, "", rest, nil, true)
		return
	}
	var control *controlRequestError
	if errors.As(err, &control) {
		sendError(w, r, http.StatusBadRequest, control.message, rest, nil, control.closeConn)
		return
	}
	if storageErr, ok := model.AsStorageError(err); ok {
		switch storageErr.Kind {
		case model.KindInvalidPath:
			sendError(w, r, http.StatusBadRequest, storageErr.Message, rest, nil, false)
		case model.KindEntryNotFound:
			sendError(w, r, http.StatusNotFound, storageErr.Message, rest, nil, false)
		case model.KindNotFolder, model.KindAlreadyExists, model.KindAmbiguousPath:
			sendError(w, r, http.StatusConflict, storageErr.Message, rest, nil, false)
		case model.KindInsufficientStorage:
			sendError(w, r, http.StatusInsufficientStorage, storageErr.Message, rest, nil, false)
		case model.KindServiceBusy:
			sendError(w, r, http.StatusServiceUnavailable, storageErr.Message, rest,
				map[string]string{"Retry-After": "5"}, false)
		case model.KindUnsupportedOperation:
			sendError(w, r, http.StatusNotImplemented, storageErr.Message, rest, nil, false)
		case model.KindBadRequest:
			// Python maps bare ValueError/TypeError to 400 with the exact
			// message; the kind keeps that mapping text-free of heuristics.
			sendError(w, r, http.StatusBadRequest, storageErr.Message, rest, nil, false)
		case model.KindIOFailure:
			// Python maps OSError to a fixed 502 message: the underlying
			// filesystem or transport detail never reaches the client.
			sendError(w, r, http.StatusBadGateway, "local or upstream I/O failed", rest, nil, false)
		default:
			sendError(w, r, http.StatusInternalServerError, "internal server error", rest, nil, false)
		}
		return
	}
	if wpsErr, ok := model.AsWpsAPIError(err); ok {
		mapWpsError(w, r, wpsErr, rest)
		return
	}
	var settingsErr *workspace.SettingsError
	if errors.As(err, &settingsErr) {
		// WebSettingsError extends ValueError in Python: 400 with the
		// validation message.
		sendError(w, r, http.StatusBadRequest, settingsErr.Msg, rest, nil, false)
		return
	}
	var settingsFileErr *workspace.SettingsFileError
	if errors.As(err, &settingsFileErr) {
		// WebSettingsFileError extends OSError: a fixed message, never the
		// underlying detail.
		sendError(w, r, http.StatusBadGateway, "local or upstream I/O failed", rest, nil, false)
		return
	}
	// Python's final fallback logs and answers a fixed 500.
	sendError(w, r, http.StatusInternalServerError, "internal server error", rest, nil, false)
}

// wpsErrorPayload keeps the Python payload key order (error, code,
// upstream_status) so REST error bodies stay byte-identical.
type wpsErrorPayload struct {
	Error          string `json:"error"`
	Code           string `json:"code"`
	UpstreamStatus *int   `json:"upstream_status,omitempty"`
}

func mapWpsError(w http.ResponseWriter, r *http.Request, wpsErr *model.WpsAPIError, rest bool) {
	status := http.StatusBadGateway
	message := "upstream WPS request failed"
	code := "wps_unavailable"
	extra := map[string]string{}
	if wpsErr.Status == 401 {
		// Never relay whether credentials exist or which failed; the fixed
		// code only says the session needs a refresh.
		status = http.StatusServiceUnavailable
		message = "WPS session expired; refresh the configured credentials"
		code = "wps_session_expired"
		extra["Retry-After"] = "60"
	}
	if rest {
		payload := wpsErrorPayload{Error: message, Code: code}
		if wpsErr.Status != 0 {
			upstream := wpsErr.Status
			payload.UpstreamStatus = &upstream
		}
		if err := sendJSON(w, r, status, payload, DefaultControlLimits(), extra); err != nil {
			// The payload is a few bytes; sendJSON cannot exceed the limit
			// here, but stay safe instead of recursing.
			writeResponse(w, r, status, []byte(message+"\n"), contentTypeText, extra, false)
		}
		return
	}
	sendError(w, r, status, message, false, extra, false)
}
