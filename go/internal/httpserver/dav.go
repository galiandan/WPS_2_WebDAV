package httpserver

import (
	"fmt"
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
	uploads   UploadStorage
	// maxUploadBytes mirrors the declared-upload gate reading
	// client.config.max_upload_bytes; zero disables the check like the
	// Python getattr fallback.
	maxUploadBytes int64
	davPrefix      string
}

// NewDAVDispatcher wires the dispatcher. Zero limits select the Python
// AdapterApplication defaults (1 MiB / 16 MiB control bounds, 10000
// PROPFIND entries, depth 64, 1 MiB download chunks); the prefix is
// trimmed like Python's dav_prefix.rstrip("/") before href building. The
// download and upload surfaces are the same storage, but they are declared
// separately so test fakes can scope what each route observes.
func NewDAVDispatcher(storage DAVStorage, limits ControlLimits, propfind DAVLimits, download DownloadLimits, downloads DownloadStorage, uploads UploadStorage, maxUploadBytes int64, davPrefix string) (*DAVDispatcher, error) {
	if storage == nil {
		return nil, errChainConfig("a storage is required")
	}
	if downloads == nil {
		return nil, errChainConfig("a download storage is required")
	}
	if uploads == nil {
		return nil, errChainConfig("an upload storage is required")
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
		storage:        storage,
		limits:         limits,
		propfind:       propfind,
		download:       download,
		downloads:      downloads,
		uploads:        uploads,
		maxUploadBytes: maxUploadBytes,
		davPrefix:      strings.TrimRight(davPrefix, "/"),
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
	case "PUT":
		return d.doDavPut(w, r, davPath)
	default:
		sendUnknownRoute(w, r)
		return nil
	}
}

// doHead mirrors do_HEAD: directories answer with the fixed directory
// content type and length zero; files answer through _send_download's head
// branch — metadata only, the object body is never opened. Range and
// If-Range share the GET prelude (B802): a matching ETag yields 206 with
// Content-Range, otherwise the full entry is reported and unsatisfiable
// ranges answer 416.
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
	offset, length, rangeRequested, ok := resolveRange(w, r, entry, false)
	if !ok {
		return nil
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
	if rangeRequested {
		header["Content-Range"] = []string{fmt.Sprintf("bytes %d-%d/%d", offset, offset+*length-1, *entry.Size)}
		header["Content-Length"] = []string{strconv.FormatInt(*length, 10)}
	} else if entry.Size != nil && *entry.Size >= 0 {
		// Python adds Content-Length when the size is known and non-negative;
		// an unknown size relies on the close framing below.
		header.Set("Content-Length", strconv.FormatInt(*entry.Size, 10))
	}
	// Python marks every download response for closing, HEAD included.
	header.Set("Connection", "close")
	status := http.StatusOK
	if rangeRequested {
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	return nil
}
