package app

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/web"
)

func gzipAuthHeaders(extra map[string]string) map[string]string {
	headers := map[string]string{
		"Authorization":   authHeaders["Authorization"],
		"Accept-Encoding": "gzip, deflate, br, zstd",
	}
	for name, value := range extra {
		headers[name] = value
	}
	return headers
}

func decodeGzipBody(t *testing.T, body []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("response body is not valid gzip: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("gzip body does not decompress: %v", err)
	}
	return decoded
}

func TestWebResourcesServeGzipWhenAccepted(t *testing.T) {
	cfg := withAuth(t, fixtureConfig(t))
	server, _ := newTestServer(t, cfg)
	targets := []struct {
		path     string
		identity []byte
		etag     string
	}{
		{"/", web.Page(), web.PageETag()},
		{"/assets/style.css", mustAssetBytes(t, "style.css"), mustAssetETag(t, "style.css")},
		{"/assets/app.js", mustAssetBytes(t, "app.js"), mustAssetETag(t, "app.js")},
	}
	for _, target := range targets {
		response := get(t, server.URL+target.path, gzipAuthHeaders(nil))
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s = %d", target.path, response.StatusCode)
			continue
		}
		if encoding := response.Header.Get("Content-Encoding"); encoding != "gzip" {
			t.Errorf("%s Content-Encoding = %q, want gzip", target.path, encoding)
		}
		if vary := response.Header.Get("Vary"); vary != "Accept-Encoding" {
			t.Errorf("%s Vary = %q, want Accept-Encoding", target.path, vary)
		}
		if etag := response.Header.Get("ETag"); etag != target.etag {
			t.Errorf("%s ETag = %q, want the identity validator %q", target.path, etag, target.etag)
		}
		if cl := response.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
			t.Errorf("%s Content-Length = %q, want the gzip body length %d", target.path, cl, len(body))
		}
		if len(body) >= len(target.identity) {
			t.Errorf("%s gzip body (%d bytes) is not smaller than identity (%d bytes)", target.path, len(body), len(target.identity))
		}
		if decoded := decodeGzipBody(t, body); string(decoded) != string(target.identity) {
			t.Errorf("%s gzip body decodes to different bytes than the embedded resource", target.path)
		}
	}
}

func TestWebResourcesStayIdentityWithoutAcceptEncoding(t *testing.T) {
	cfg := withAuth(t, fixtureConfig(t))
	server, _ := newTestServer(t, cfg)
	identity := []struct {
		path     string
		identity []byte
	}{
		{"/", web.Page()},
		{"/assets/style.css", mustAssetBytes(t, "style.css")},
	}
	for _, target := range identity {
		response := get(t, server.URL+target.path, authHeaders)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s = %d", target.path, response.StatusCode)
			continue
		}
		if encoding := response.Header.Get("Content-Encoding"); encoding != "" {
			t.Errorf("%s unexpectedly carries Content-Encoding %q", target.path, encoding)
		}
		if vary := response.Header.Get("Vary"); vary != "" {
			t.Errorf("%s unexpectedly carries Vary %q", target.path, vary)
		}
		if string(body) != string(target.identity) {
			t.Errorf("%s body differs from the embedded resource", target.path)
		}
	}
}

func TestWebResourcesRevalidateWithETagUnderGzip(t *testing.T) {
	cfg := withAuth(t, fixtureConfig(t))
	server, _ := newTestServer(t, cfg)
	targets := []struct {
		path string
		etag string
	}{
		{"/", web.PageETag()},
		{"/assets/style.css", mustAssetETag(t, "style.css")},
		{"/assets/app.js", mustAssetETag(t, "app.js")},
	}
	for _, target := range targets {
		response := get(t, server.URL+target.path, gzipAuthHeaders(map[string]string{
			"If-None-Match": target.etag,
		}))
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusNotModified {
			t.Errorf("%s = %d, want 304", target.path, response.StatusCode)
		}
		if len(body) != 0 {
			t.Errorf("%s 304 carries a body", target.path)
		}
		if encoding := response.Header.Get("Content-Encoding"); encoding != "" {
			t.Errorf("%s 304 unexpectedly carries Content-Encoding %q", target.path, encoding)
		}
		if etag := response.Header.Get("ETag"); etag != target.etag {
			t.Errorf("%s 304 ETag = %q, want %q", target.path, etag, target.etag)
		}
	}
}

func mustAssetBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, _, ok := web.Asset(name)
	if !ok {
		t.Fatalf("asset %q missing from the embed manifest", name)
	}
	return data
}
