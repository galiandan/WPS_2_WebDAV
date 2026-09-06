package httpserver

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// DAVStorage is the storage surface the WebDAV methods share. The root
// answer uses Metadata/ListPath (multi-space routing by request path);
// the PROPFIND walk descends by parent ID through ListChildren so deeper
// levels never re-resolve from the root (B703).
type DAVStorage interface {
	RESTReadStorage
	ListChildren(scopePath string, entry model.RemoteEntry) ([]model.RemoteEntry, error)
}

// DAVLimits mirrors the AdapterApplication PROPFIND bounds.
type DAVLimits struct {
	MaxPropfindEntries int
	MaxPropfindDepth   int
}

// DAVDispatcher routes the WebDAV methods under the DAV prefix. B701
// delivers HEAD, B702/B703 PROPFIND, and B801 the streaming GET; the write
// methods land with their stages — until then every unimplemented method
// answers the router's unknown-route fallback exactly like an
// unimplemented do_* would.
type DAVDispatcher struct {
	storage   DAVStorage
	limits    ControlLimits
	propfind  DAVLimits
	download  DownloadLimits
	downloads DownloadStorage
	davPrefix string
}

// NewDAVDispatcher wires the dispatcher. Zero limits select the Python
// AdapterApplication defaults (1 MiB / 16 MiB control bounds, 10000
// PROPFIND entries, depth 64, 1 MiB download chunks); the prefix is
// trimmed like Python's dav_prefix.rstrip("/") before href building. The
// download surface is the same storage, but it is declared separately so
// test fakes can scope what each route observes.
func NewDAVDispatcher(storage DAVStorage, limits ControlLimits, propfind DAVLimits, download DownloadLimits, downloads DownloadStorage, davPrefix string) (*DAVDispatcher, error) {
	if storage == nil {
		return nil, errChainConfig("a storage is required")
	}
	if downloads == nil {
		return nil, errChainConfig("a download storage is required")
	}
	if limits.MaxControlBody <= 0 || limits.MaxResponseBody <= 0 {
		limits = DefaultControlLimits()
	}
	if propfind.MaxPropfindEntries <= 0 {
		propfind.MaxPropfindEntries = 10000
	}
	if propfind.MaxPropfindDepth <= 0 {
		propfind.MaxPropfindDepth = 64
	}
	return &DAVDispatcher{
		storage:   storage,
		limits:    limits,
		propfind:  propfind,
		download:  download,
		downloads: downloads,
		davPrefix: strings.TrimRight(davPrefix, "/"),
	}, nil
}

// ServeDAV fits Handlers.DAV in the router; returned errors map through
// the domain table with the plain-text framing.
func (d *DAVDispatcher) ServeDAV(w http.ResponseWriter, r *http.Request, davPath string) error {
	switch r.Method {
	case "GET":
		return sendDownload(w, r, davPath, false, d.downloads, d.download.chunkSize())
	case "HEAD":
		return d.doHead(w, r, davPath)
	case "PROPFIND":
		return d.doPropfind(w, r, davPath)
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
