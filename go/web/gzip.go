package web

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// The three embedded assets are immutable for the lifetime of the binary,
// so their gzip encodings are computed once and reused for every request:
// the first page load transfers roughly a quarter of the identity bytes,
// and later loads keep revalidating against the same identity ETag.
const gzipLevel = gzip.BestCompression

var pageGzip = sync.OnceValue(func() []byte { return gzipBytes(Page()) })

// PageGzip returns the gzip encoding of the embedded page bytes.
func PageGzip() []byte { return pageGzip() }

var assetGzips = sync.OnceValue(func() map[string][]byte {
	variants := make(map[string][]byte, len(assetContentTypes))
	for name, contentType := range assetContentTypes {
		// Compress text assets only; already-compressed media (JPEG) gains
		// nothing from a precomputed gzip variant.
		if !strings.HasPrefix(contentType, "text/") {
			continue
		}
		if data, _, ok := Asset(name); ok {
			variants[name] = gzipBytes(data)
		}
	}
	return variants
})

// AssetGzip returns the gzip encoding of one whitelisted asset. The bool
// mirrors Asset: names outside the fixed manifest never carry a variant.
func AssetGzip(name string) ([]byte, bool) {
	data, ok := assetGzips()[name]
	return data, ok
}

func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(data) / 2)
	writer, err := gzip.NewWriterLevel(&buf, gzipLevel)
	if err != nil {
		return nil
	}
	if _, err := writer.Write(data); err != nil {
		return nil
	}
	if err := writer.Close(); err != nil {
		return nil
	}
	if buf.Len() == 0 || buf.Len() >= len(data) {
		return nil
	}
	return buf.Bytes()
}

// ClientAcceptsGzip reports whether the request's Accept-Encoding field
// lists gzip with a non-zero quality. A missing field, other codings, or an
// unparsable quality all fall back to identity: sending the uncompressed
// bytes is always a valid choice.
func ClientAcceptsGzip(r *http.Request) bool {
	header := r.Header.Get("Accept-Encoding")
	if header == "" {
		return false
	}
	for _, field := range strings.Split(header, ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(field), ";")
		if !strings.EqualFold(strings.TrimSpace(token), "gzip") {
			continue
		}
		quality := "1"
		for _, param := range strings.Split(params, ";") {
			name, value, _ := strings.Cut(strings.TrimSpace(param), "=")
			if strings.EqualFold(name, "q") {
				quality = strings.TrimSpace(value)
			}
		}
		if value, err := strconv.ParseFloat(quality, 64); err == nil && value <= 0 {
			return false
		}
		return true
	}
	return false
}

// NegotiateGzip picks the representation for one asset response: the gzip
// variant only when the client advertises gzip and the variant is actually
// smaller; otherwise the identity bytes. The returned encoding name is
// empty for identity. The ETag stays the identity validator in both cases —
// it names the entity, and Vary: Accept-Encoding (set by the caller when the
// gzip variant is chosen) keeps caches from mixing encodings.
func NegotiateGzip(r *http.Request, identity []byte, gzipped []byte) ([]byte, string) {
	if len(gzipped) == 0 || len(gzipped) >= len(identity) || !ClientAcceptsGzip(r) {
		return identity, ""
	}
	return gzipped, "gzip"
}
