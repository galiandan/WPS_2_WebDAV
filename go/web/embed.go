// Package web embeds the three fixed frontend assets: index.html,
// style.css, and app.js. The bytes are compiled into the service binary;
// nothing here reads configuration, touches a disk path, or rewrites the
// page at runtime — the root name reaches the page through the settings
// API, never through template substitution.
//
// Cache policy: the file names carry no
// content hash, so every asset — page, style, and script — is served
// no-store to keep upgraded binaries from mixing old resources with new
// HTML.
package web

import (
	"embed"
)

//go:embed index.html style.css app.js
var files embed.FS

// CacheControl is the fixed cache policy for every web asset.
const CacheControl = "no-store"

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
