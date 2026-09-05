package httpserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMarshalPythonJSON pins the ensure_ascii compact rendering that REST
// payloads must match byte for byte.
func TestMarshalPythonJSON(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"ascii", map[string]string{"error": "unknown REST route"}, `{"error":"unknown REST route"}`},
		{"compact", map[string][]int{"a": {1, 2}}, `{"a":[1,2]}`},
		{"html-raw", map[string]string{"m": "<a>&"}, `{"m":"<a>&"}`},
		{"non-ascii", map[string]string{"m": "名前"}, `{"m":"\u540d\u524d"}`},
		{"del", map[string]string{"m": "\x7f"}, `{"m":"\u007f"}`},
		{"control", map[string]string{"m": "\x01\n"}, `{"m":"\u0001\n"}`},
		{"emoji", map[string]string{"m": "😀"}, `{"m":"\ud83d\ude00"}`},
		{"line-sep", map[string]string{"m": "\u2028"}, `{"m":"\u2028"}`},
	}
	for _, tc := range cases {
		got, err := marshalPythonJSON(tc.value)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func newTestRequest(method, target string) *http.Request {
	return httptest.NewRequest(method, target, nil)
}

// TestWriteResponseHeaders pins the _send_bytes header surface.
func TestWriteResponseHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeResponse(recorder, newTestRequest("GET", "/"), http.StatusNotFound, []byte("nope\n"), contentTypeText, nil, false)
	got := recorder.Header()
	if got.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got.Get("Content-Type"))
	}
	if got.Get("Content-Length") != "5" {
		t.Errorf("Content-Length = %q", got.Get("Content-Length"))
	}
	if got.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", got.Get("Cache-Control"))
	}
	if got.Get("Connection") != "" {
		t.Errorf("unexpected Connection header %q", got.Get("Connection"))
	}
	if body := recorder.Body.String(); body != "nope\n" {
		t.Errorf("body = %q", body)
	}
}

// TestWriteResponseSuppressesBodyForHead mirrors _send_bytes writing headers
// but no payload for HEAD.
func TestWriteResponseSuppressesBodyForHead(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeResponse(recorder, newTestRequest("HEAD", "/"), http.StatusOK, []byte("payload"), contentTypeText, nil, false)
	if recorder.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", recorder.Body.String())
	}
	if recorder.Header().Get("Content-Length") != "7" {
		t.Errorf("HEAD Content-Length = %q, want 7", recorder.Header().Get("Content-Length"))
	}
}

// TestWriteResponseConnectionClose covers both the closeConn flag and an
// explicit Connection header suppressing the automatic one.
func TestWriteResponseConnectionClose(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeResponse(recorder, newTestRequest("GET", "/"), http.StatusForbidden, nil, contentTypeText, nil, true)
	if recorder.Header().Get("Connection") != "close" {
		t.Errorf("Connection = %q, want close", recorder.Header().Get("Connection"))
	}

	recorder = httptest.NewRecorder()
	writeResponse(recorder, newTestRequest("GET", "/"), http.StatusForbidden, nil, contentTypeText,
		map[string]string{"connection": "close"}, true)
	if count := len(recorder.Header().Values("Connection")); count != 1 {
		t.Errorf("Connection header count = %d, want 1", count)
	}
}

// TestSendErrorFormats pins both error shapes: text plus newline for DAV,
// compact JSON for REST.
func TestSendErrorFormats(t *testing.T) {
	recorder := httptest.NewRecorder()
	sendError(recorder, newTestRequest("GET", "/dav/x"), http.StatusNotFound, "unknown route", false, nil, true)
	if body := recorder.Body.String(); body != "unknown route\n" {
		t.Errorf("text error body = %q", body)
	}
	if recorder.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("text error Content-Type = %q", recorder.Header().Get("Content-Type"))
	}
	if recorder.Header().Get("Connection") != "close" {
		t.Errorf("text error Connection = %q", recorder.Header().Get("Connection"))
	}

	recorder = httptest.NewRecorder()
	sendError(recorder, newTestRequest("GET", "/api/v1/x"), http.StatusNotFound, "unknown REST route", true, nil, false)
	if body := recorder.Body.String(); body != `{"error":"unknown REST route"}` {
		t.Errorf("REST error body = %q", body)
	}
	if recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("REST error Content-Type = %q", recorder.Header().Get("Content-Type"))
	}
	if recorder.Header().Get("Connection") != "" {
		t.Errorf("REST error unexpectedly closes: %q", recorder.Header().Get("Connection"))
	}
}

// Fuzz-ish sanity: the ASCII escape must always produce valid JSON whose
// parsed value matches the unescaped payload.
func TestMarshalPythonJSONRoundTrip(t *testing.T) {
	for _, s := range []string{"plain", "名前", "mix 名 & <x> \x7f", "😀", "\u2028\u2029", "tab\there"} {
		payload, err := marshalPythonJSON(map[string]string{"m": s})
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if !bytes.HasSuffix(payload, []byte(`"}`)) || !bytes.HasPrefix(payload, []byte(`{"m":"`)) {
			t.Fatalf("%q: unexpected shape %s", s, payload)
		}
		if strings.ContainsRune(string(payload), '\n') {
			t.Fatalf("%q: payload must stay on one line: %s", s, payload)
		}
	}
}
