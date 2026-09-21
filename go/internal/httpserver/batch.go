package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

const maxSelectionPaths = 100

type batchCopier interface {
	CopyPath(context.Context, string, string, storage.CopyOptions) (model.RemoteEntry, error)
}

type batchResult struct {
	Path   string `json:"path"`
	OK     bool   `json:"ok"`
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
}

type batchResponse struct {
	Results   []batchResult `json:"results"`
	Succeeded int           `json:"succeeded"`
	Failed    int           `json:"failed"`
}

// selectionPaths rejects aliases, roots and overlapping selections before any
// work starts. JSON paths are business paths and must not be URL-decoded again.
func selectionPaths(value any) ([]string, error) {
	values, ok := value.([]any)
	if !ok || len(values) == 0 || len(values) > maxSelectionPaths {
		return nil, errBadRequest("select between 1 and 100 paths")
	}
	paths := make([]string, 0, len(values))
	for _, value := range values {
		raw, ok := value.(string)
		if !ok {
			return nil, errBadRequest("selected paths must be strings")
		}
		canonical, err := canonicalRemotePath(raw)
		if err != nil {
			return nil, err
		}
		if canonical == "/" {
			return nil, errBadRequest("select entries inside the root")
		}
		for _, previous := range paths {
			if previous == canonical || strings.HasPrefix(previous, canonical+"/") || strings.HasPrefix(canonical, previous+"/") {
				return nil, errBadRequest("selected paths must not overlap")
			}
		}
		paths = append(paths, canonical)
	}
	return paths, nil
}

func (d *RESTDispatcher) doBatch(w http.ResponseWriter, r *http.Request) error {
	payload, err := readJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	operation, _ := payload["operation"].(string)
	if operation != "copy" && operation != "move" && operation != "delete" {
		return errBadRequest("operation must be copy, move or delete")
	}
	for key := range payload {
		if key != "operation" && key != "paths" && (key != "destination" || operation == "delete") {
			return errBadRequest("unknown batch field")
		}
	}
	paths, err := selectionPaths(payload["paths"])
	if err != nil {
		return err
	}
	destination := ""
	if operation != "delete" {
		destination, _ = payload["destination"].(string)
		destination, err = canonicalRemotePath(destination)
		if err != nil {
			return err
		}
		parent, err := d.read.Metadata(destination)
		if err != nil {
			return err
		}
		if parent.Kind != model.KindFolder {
			return model.NewStorageError(model.KindNotFolder, "destination is not a folder")
		}
		names := make(map[string]bool, len(paths))
		for _, source := range paths {
			name := path.Base(source)
			if names[name] {
				return errBadRequest("selected entries have duplicate destination names")
			}
			names[name] = true
			if destination == source || strings.HasPrefix(destination, source+"/") {
				return errBadRequest("destination must not be inside a selected entry")
			}
		}
	}
	response := batchResponse{Results: make([]batchResult, 0, len(paths))}
	tokens := TokensFromHeaders(r.Header.Get("If"), r.Header.Get("Lock-Token"))
	for _, source := range paths {
		// Stop starting work after the browser disconnects. A completed item
		// is never rolled back or retried implicitly.
		if r.Context().Err() != nil {
			return nil
		}
		result := batchResult{Path: source, Status: http.StatusOK}
		target := ""
		if destination != "" {
			target = strings.TrimSuffix(destination, "/") + "/" + path.Base(source)
		}
		if !d.locks.allowsTree(source, tokens) || (target != "" && (!d.locks.Allows(destination, tokens) || !d.locks.allowsTree(target, tokens))) {
			result.Status, result.Error = http.StatusLocked, "resource is locked"
		} else {
			err = d.applyBatchItem(r.Context(), operation, source, target)
			if err != nil {
				result = batchFailure(r, source, err)
			} else {
				result.OK = true
			}
		}
		response.Results = append(response.Results, result)
		if result.OK {
			response.Succeeded++
		} else {
			response.Failed++
		}
	}
	return sendJSON(w, r, http.StatusOK, response, d.limits, nil)
}

func (d *RESTDispatcher) applyBatchItem(ctx context.Context, operation, source, destination string) error {
	if operation == "delete" {
		return d.mutations.DeletePath(source)
	}
	if _, err := d.read.Metadata(destination); err == nil {
		return model.NewStorageError(model.KindAlreadyExists, "destination already exists")
	} else if !storageEntryNotFound(err) {
		return err
	}
	if operation == "move" {
		_, err := d.mutations.MovePath(source, destination)
		return err
	}
	copier, ok := d.mutations.(batchCopier)
	if !ok {
		return model.NewStorageError(model.KindUnsupportedOperation, "copy is unavailable")
	}
	_, err := copier.CopyPath(ctx, source, destination, storage.CopyOptions{Depth: "infinity", Overwrite: false})
	return err
}

// Reuse the established status table and redaction for item errors rather than
// returning raw upstream errors or maintaining a second mapping.
type batchErrorResponse struct {
	header http.Header
	status int
	bytes.Buffer
}

func (w *batchErrorResponse) Header() http.Header    { return w.header }
func (w *batchErrorResponse) WriteHeader(status int) { w.status = status }

func batchFailure(r *http.Request, source string, err error) batchResult {
	w := &batchErrorResponse{header: make(http.Header)}
	mapError(w, r, err, true)
	payload := struct {
		Error string `json:"error"`
	}{}
	_ = json.Unmarshal(w.Bytes(), &payload)
	return batchResult{Path: source, Status: w.status, Error: payload.Error}
}

// A folder move/delete also affects locks on descendants. Checking only the
// folder path would allow a batch request to remove a separately locked file.
func (s *DavLockStore) allowsTree(path string, tokens map[string]struct{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purge()
	for _, lock := range s.locks {
		if lockApplies(lock, path) || strings.HasPrefix(lock.Path, strings.TrimSuffix(path, "/")+"/") {
			if _, ok := tokens[lock.Token]; !ok {
				return false
			}
		}
	}
	return true
}
