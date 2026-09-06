package httpserver

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// DAVStorage is the metadata surface the WebDAV methods share. Path
// resolution and listing arrive with the PROPFIND stages; HEAD needs only
// Metadata because a HEAD answer never reads the object body.
type DAVStorage interface {
	Metadata(path string) (model.RemoteEntry, error)
}

// DAVDispatcher routes the WebDAV methods under the DAV prefix. B701
// delivers HEAD; PROPFIND lands with B702/B703, GET with the download
// stage, and the write methods with their stages — until then every other
// method answers the router's unknown-route fallback exactly like an
// unimplemented do_* would.
type DAVDispatcher struct {
	storage DAVStorage
}

// NewDAVDispatcher wires the dispatcher.
func NewDAVDispatcher(storage DAVStorage) (*DAVDispatcher, error) {
	if storage == nil {
		return nil, errChainConfig("a storage is required")
	}
	return &DAVDispatcher{storage: storage}, nil
}

// ServeDAV fits Handlers.DAV in the router; returned errors map through
// the domain table with the plain-text framing.
func (d *DAVDispatcher) ServeDAV(w http.ResponseWriter, r *http.Request, davPath string) error {
	switch r.Method {
	case "HEAD":
		return d.doHead(w, r, davPath)
	default:
		sendUnknownRoute(w, r)
		return nil
	}
}

// doHead mirrors do_HEAD: directories answer with the fixed directory
// content type and length zero; files answer through _send_download's
// head branch — metadata only, the object body is never opened. Range and
// If-Range handling (206/416) arrives with B802; until then a Range
// header is ignored and the full entry is reported.
func (d *DAVDispatcher) doHead(w http.ResponseWriter, r *http.Request, davPath string) error {
	entry, err := d.storage.Metadata(davPath)
	if err != nil {
		return err
	}
	header := w.Header()
	if entry.Kind == model.KindFolder {
		// Python writes directory HEADs with raw send_response calls: no
		// Cache-Control, no ETag, no capability headers — just the type
		// and the zero length.
		header.Set("Content-Type", "httpd/unix-directory")
		header.Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return nil
	}
	if entry.Kind != model.KindFile {
		return model.NewStorageError(model.KindNotFolder, "the requested path is not a file")
	}
	header.Set("Content-Type", guessMimeType(entry.Name))
	header.Set("Accept-Ranges", "bytes")
	header.Set("Cache-Control", "no-store, no-transform")
	header.Set("X-Content-Type-Options", "nosniff")
	if entry.Etag != nil && *entry.Etag != "" {
		// Python quotes with f'"{etag.strip(chr(34))}"': strip every
		// leading/trailing quote, then wrap the remainder exactly once.
		// The key is assigned directly — Go's canonicalization would
		// rename it to "Etag" on the wire while Python sends "ETag".
		header["ETag"] = []string{`"` + strings.Trim(*entry.Etag, `"`) + `"`}
	}
	// Python adds Content-Length when the size is known and non-negative;
	// an unknown size relies on the close framing below.
	if entry.Size != nil && *entry.Size >= 0 {
		header.Set("Content-Length", strconv.FormatInt(*entry.Size, 10))
	}
	// Python marks every download response for closing, HEAD included.
	header.Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	return nil
}
