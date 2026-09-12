// Package web embeds the three fixed frontend assets: index.html,
// style.css, and app.js. The bytes are compiled into the service binary;
// nothing here reads configuration, touches a disk path, or rewrites the
// page at runtime — the root name reaches the page through the settings
// API, never through template substitution.
//
// Cache policy: the file names carry no content hash, so a stored copy may
// only be reused after the server confirms it still matches this build.
// Every asset — page, style, and script — is served no-cache with a strong
// content-derived ETag: each visit revalidates, the browser keeps the bytes
// on a header-only 304, and downloads the new version the moment the binary
// ships different content. An upgraded binary therefore never mixes old
// resources with new HTML: every cached copy is validated against the
// running build before use.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"sync"
)

//go:embed index.html style.css app.js
var files embed.FS

// CacheControl is the fixed cache policy for every web asset: store freely,
// but revalidate before every reuse.
const CacheControl = "no-cache"

// assetContentTypes is the whitelist; anything outside it is never served.
var assetContentTypes = map[string]string{
	"app.js":    "text/javascript; charset=utf-8",
	"style.css": "text/css; charset=utf-8",
}

// Page returns the index.html bytes. With //go:embed the file exists at
// build time, so the read cannot fail.
func Page() []byte {
	data, err := files.ReadFile("index.html")
	if err != nil {
		return nil
	}
	return data
}

// Asset resolves one whitelisted static asset name to its bytes and MIME
// type. Any other name — including paths, encoded spellings, and the empty
// name — reports false, so only the fixed manifest is ever exposed.
func Asset(name string) ([]byte, string, bool) {
	contentType, ok := assetContentTypes[name]
	if !ok {
		return nil, "", false
	}
	data, err := files.ReadFile(name)
	if err != nil {
		return nil, "", false
	}
	return data, contentType, true
}

// contentETag derives a strong entity tag from the exact bytes: any change
// in any build output changes the validator, byte-for-byte copies keep it.
func contentETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"sha256-` + hex.EncodeToString(sum[:]) + `"`
}

var pageETag = sync.OnceValue(func() string { return contentETag(Page()) })

// PageETag returns the strong validator of the embedded page bytes.
func PageETag() string { return pageETag() }

var assetETags = sync.OnceValue(func() map[string]string {
	tags := make(map[string]string, len(assetContentTypes))
	for name := range assetContentTypes {
		if data, _, ok := Asset(name); ok {
			tags[name] = contentETag(data)
		}
	}
	return tags
})

// AssetETag returns the strong validator of one whitelisted asset. The bool
// mirrors Asset: names outside the fixed manifest never carry an ETag.
func AssetETag(name string) (string, bool) {
	tag, ok := assetETags()[name]
	return tag, ok
}
