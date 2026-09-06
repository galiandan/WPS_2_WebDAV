package httpserver

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

// UploadStorage is the storage surface the upload routes share: both the
// single-space Storage and the MultiSpace view expose UploadPath. Declared
// separately so test fakes can scope what each route observes.
type UploadStorage interface {
	UploadPath(ctx context.Context, path string, source io.Reader, options storage.UploadOptions) (model.RemoteEntry, error)
}

// checkDeclaredUploadLength mirrors _check_declared_upload_length: a
// declared body above the configured upload maximum is refused with 507
// over a closed connection before the first body byte is read. Python
// reads the limit through the storage's client config; app wiring passes
// the same static value here. The return reports whether the upload may
// continue — a refused request is already answered.
func checkDeclaredUploadLength(w http.ResponseWriter, r *http.Request, length int64, maxUploadBytes int64, rest bool) bool {
	if maxUploadBytes > 0 && length > maxUploadBytes {
		sendError(w, r, http.StatusInsufficientStorage, "upload exceeds the configured size limit", rest, nil, true)
		return false
	}
	return true
}

// queryBool mirrors _query_bool: exactly one value, case-insensitive truthy
// and falsy sets, and an InvalidPathError (400) otherwise.
func queryBool(query url.Values, name string) (bool, error) {
	values, ok := query[name]
	if !ok {
		return false, nil
	}
	if len(values) != 1 {
		return false, model.NewStorageError(model.KindInvalidPath, "query parameter '"+name+"' must contain one value")
	}
	switch strings.ToLower(strings.TrimSpace(values[0])) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, model.NewStorageError(model.KindInvalidPath, "query parameter '"+name+"' must be boolean")
}

// doRestPut mirrors _do_rest_put for the upload/files routes: the declared
// length gates (411 then 507) run before the path and overwrite query are
// parsed, and every upload failure drains through the domain error table.
func (d *RESTDispatcher) doRestPut(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	length, err := contentLength(w, r, true)
	if err != nil || length == nil {
		return err
	}
	if !checkDeclaredUploadLength(w, r, *length, d.maxUploadBytes, true) {
		return nil
	}
	path, err := queryPath(route.Query)
	if err != nil {
		return err
	}
	overwrite, err := queryBool(route.Query, "overwrite")
	if err != nil {
		return err
	}
	entry, err := d.uploads.UploadPath(r.Context(), path, r.Body, storage.UploadOptions{
		Size:        length,
		ContentType: r.Header.Get("Content-Type"),
		Overwrite:   overwrite,
	})
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusCreated, uploadPayload{Path: path, Entry: entry.Public()}, d.limits, nil)
}

// uploadPayload keeps the Python response key order (path, entry).
type uploadPayload struct {
	Path  string            `json:"path"`
	Entry model.PublicEntry `json:"entry"`
}

// doDavPut mirrors _do_webdav_put: the length gates run before the upload,
// overwrite is always on, and success answers 201 with the entry JSON and
// the quoted Location href.
func (d *DAVDispatcher) doDavPut(w http.ResponseWriter, r *http.Request, davPath string) error {
	length, err := contentLength(w, r, true)
	if err != nil || length == nil {
		return err
	}
	if !checkDeclaredUploadLength(w, r, *length, d.maxUploadBytes, false) {
		return nil
	}
	entry, err := d.uploads.UploadPath(r.Context(), davPath, r.Body, storage.UploadOptions{
		Size:        length,
		ContentType: r.Header.Get("Content-Type"),
		Overwrite:   true,
	})
	if err != nil {
		return err
	}
	parts, err := storage.SplitRemotePath(davPath)
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusCreated, entry.Public(), d.limits, map[string]string{
		"Location": buildHref(parts, entry, d.davPrefix),
	})
}
