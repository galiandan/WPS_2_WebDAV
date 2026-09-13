package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"testing"
)

func TestGzipVariantsDecodeToEmbeddedBytes(t *testing.T) {
	page := Page()
	if len(page) == 0 {
		t.Fatal("the embedded page is empty")
	}
	variants := map[string][]byte{
		"index.html": page,
		"style.css":  nil,
		"app.js":     nil,
	}
	for _, name := range []string{"style.css", "app.js"} {
		data, _, ok := Asset(name)
		if !ok {
			t.Fatalf("asset %q missing from the embed manifest", name)
		}
		variants[name] = data
	}
	for name, identity := range variants {
		var gzipped []byte
		if name == "index.html" {
			gzipped = PageGzip()
		} else {
			data, ok := AssetGzip(name)
			if !ok {
				t.Fatalf("asset %q has no gzip variant", name)
			}
			gzipped = data
		}
		if gzipped == nil {
			t.Fatalf("%s produced no gzip variant", name)
		}
		if len(gzipped) >= len(identity) {
			t.Errorf("%s gzip variant (%d bytes) is not smaller than identity (%d bytes)", name, len(gzipped), len(identity))
		}
		reader, err := gzip.NewReader(bytes.NewReader(gzipped))
		if err != nil {
			t.Fatalf("%s gzip variant is not valid gzip: %v", name, err)
		}
		decoded, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("%s gzip variant does not decompress: %v", name, err)
		}
		if string(decoded) != string(identity) {
			t.Errorf("%s gzip variant decodes to different bytes", name)
		}
	}
}

func TestClientAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"identity", false},
		{"br, zstd", false},
		{"gzip", true},
		{"gzip, deflate, br, zstd", true},
		{"deflate, gzip;q=1.0, br", true},
		{"gzip;q=0.5", true},
		{"gzip;q=0", false},
		{"gzip;q=0.000, br", false},
		{"gzip;q=0, gzip", false},
		{"GZIP", true},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest("GET", "/", nil)
		if testCase.header != "" {
			request.Header.Set("Accept-Encoding", testCase.header)
		}
		if got := ClientAcceptsGzip(request); got != testCase.want {
			t.Errorf("Accept-Encoding %q = %v, want %v", testCase.header, got, testCase.want)
		}
	}
}

func TestNegotiateGzipPrefersSmallerVariantOnlyWhenAccepted(t *testing.T) {
	identity := []byte("identity bytes")
	variant := []byte("gz")

	accepted := httptest.NewRequest("GET", "/", nil)
	accepted.Header.Set("Accept-Encoding", "gzip")
	body, encoding := NegotiateGzip(accepted, identity, variant)
	if encoding != "gzip" || string(body) != "gz" {
		t.Errorf("accepted request chose %q", encoding)
	}

	rejecting := httptest.NewRequest("GET", "/", nil)
	rejecting.Header.Set("Accept-Encoding", "gzip;q=0")
	body, encoding = NegotiateGzip(rejecting, identity, variant)
	if encoding != "" || string(body) != string(identity) {
		t.Errorf("q=0 request chose %q", encoding)
	}

	silent := httptest.NewRequest("GET", "/", nil)
	body, encoding = NegotiateGzip(silent, identity, variant)
	if encoding != "" || string(body) != string(identity) {
		t.Errorf("silent request chose %q", encoding)
	}

	// A variant that fails to shrink never replaces the identity bytes.
	body, encoding = NegotiateGzip(accepted, identity, identity)
	if encoding != "" || string(body) != string(identity) {
		t.Errorf("non-smaller variant chose %q", encoding)
	}

	body, encoding = NegotiateGzip(accepted, identity, nil)
	if encoding != "" || string(body) != string(identity) {
		t.Errorf("missing variant chose %q", encoding)
	}
}
