package httpserver

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

const (
	archiveMaxEntries = 10000
	archiveMaxDepth   = 64
	archiveMaxBytes   = int64(10 << 30)
	archiveMaxNames   = 8 << 20
)

type archiveItem struct {
	path  string
	name  string
	entry model.RemoteEntry
}

type archivePlan struct {
	items     []archiveItem
	bytes     int64
	nameBytes int
	names     map[string]bool
}

func (d *RESTDispatcher) archivePaths(w http.ResponseWriter, r *http.Request, route RESTRoute) ([]string, error) {
	if r.Method == http.MethodGet {
		if err := discardBody(w, r, d.limits); err != nil {
			return nil, err
		}
		values := make([]any, len(route.Query["path"]))
		for i, value := range route.Query["path"] {
			values[i] = value
		}
		return selectionPaths(values)
	}
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "application/x-www-form-urlencoded" {
		payload, err := readJSONBody(w, r, d.limits)
		if err != nil || payload == nil {
			return nil, err
		}
		if len(payload) != 1 {
			return nil, errBadRequest("archive requires only the paths field")
		}
		return selectionPaths(payload["paths"])
	}
	// A native form submission lets browsers stream an attachment to disk
	// without first keeping a potentially large ZIP Blob in JavaScript memory.
	length, err := contentLength(w, r, true)
	if err != nil || length == nil {
		return nil, err
	}
	if *length > d.limits.MaxControlBody {
		return nil, errRequestBodyTooLarge()
	}
	body := make([]byte, *length)
	if _, err := io.ReadFull(r.Body, body); err != nil {
		return nil, errBadRequestClose("request body is shorter than Content-Length")
	}
	values, err := url.ParseQuery(string(body))
	if err != nil || len(values) != 1 || len(values["paths"]) != 1 {
		return nil, errBadRequest("archive form requires one paths field")
	}
	var selected any
	if err := json.Unmarshal([]byte(values.Get("paths")), &selected); err != nil {
		return nil, errBadRequest("archive paths must be a JSON array")
	}
	return selectionPaths(selected)
}

func (d *RESTDispatcher) doArchive(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	paths, err := d.archivePaths(w, r, route)
	if err != nil || paths == nil {
		return err
	}
	plan, err := d.planArchive(r.Context(), paths)
	if err != nil {
		return err
	}
	name := "files.zip"
	if len(paths) == 1 {
		name = path.Base(paths[0]) + ".zip"
	}
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", `attachment; filename="download.zip"; filename*=UTF-8''`+pythonQuote(name))
	h.Set("Cache-Control", "no-store, no-transform")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	// Use normal HTTP framing: if streaming fails, aborting leaves a missing
	// final chunk (HTTP/1) or resets the stream (HTTP/2). Never close the ZIP
	// writer on failure, which would create a valid-looking partial archive.
	if err := d.writeArchive(w, r, plan); err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

func (d *RESTDispatcher) planArchive(ctx context.Context, paths []string) (*archivePlan, error) {
	plan := &archivePlan{names: make(map[string]bool)}
	for _, source := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, err := d.read.Metadata(source)
		if err != nil {
			return nil, err
		}
		if err := d.walkArchive(ctx, plan, source, path.Base(source), entry, 0, make(map[string]bool)); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func (d *RESTDispatcher) walkArchive(ctx context.Context, plan *archivePlan, source, name string, entry model.RemoteEntry, depth int, ancestors map[string]bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(plan.items) >= archiveMaxEntries || depth > archiveMaxDepth {
		return model.NewStorageError(model.KindInsufficientStorage, "archive exceeds the 10000-entry or 64-level limit")
	}
	if entry.Kind != model.KindFolder && entry.Kind != model.KindFile {
		return model.NewStorageError(model.KindUnsupportedOperation, "archive contains an unsupported entry")
	}
	// A ZIP name is an untrusted WPS name. Reuse the business path validator
	// to exclude traversal, control bytes, slashes within names and backslashes.
	if _, err := storage.SplitRemotePath("/" + name); err != nil {
		return err
	}
	if len(name) > 65534 || plan.nameBytes+len(name) > archiveMaxNames {
		return model.NewStorageError(model.KindInsufficientStorage, "archive file names exceed the size limit")
	}
	// Refuse case collisions too: extracting Foo and foo on a case-insensitive
	// filesystem must not silently discard one selected file.
	key := strings.ToLower(name)
	if plan.names[key] {
		return model.NewStorageError(model.KindAmbiguousPath, "archive contains duplicate file names")
	}
	plan.names[key] = true
	plan.nameBytes += len(name)
	if entry.Kind == model.KindFile && entry.Size != nil {
		if *entry.Size < 0 || *entry.Size > archiveMaxBytes-plan.bytes {
			return model.NewStorageError(model.KindInsufficientStorage, "archive exceeds the 10 GiB size limit")
		}
		plan.bytes += *entry.Size
	}
	plan.items = append(plan.items, archiveItem{path: source, name: name, entry: entry})
	if entry.Kind == model.KindFile {
		return nil
	}
	if entry.ID != "" {
		if ancestors[entry.ID] {
			return model.NewStorageError(model.KindAmbiguousPath, "archive folder hierarchy contains a cycle")
		}
		ancestors[entry.ID] = true
		defer delete(ancestors, entry.ID)
	}
	children, err := d.read.ListChildren(source, entry)
	if err != nil {
		return err
	}
	if len(children) > archiveMaxEntries-len(plan.items) {
		return model.NewStorageError(model.KindInsufficientStorage, "archive exceeds the 10000-entry limit")
	}
	for _, child := range children {
		if _, err := storage.JoinRemotePath([]string{child.Name}, false); err != nil {
			return err
		}
		if err := d.walkArchive(ctx, plan, source+"/"+child.Name, name+"/"+child.Name, child, depth+1, ancestors); err != nil {
			return err
		}
	}
	return nil
}

func (d *RESTDispatcher) writeArchive(w http.ResponseWriter, r *http.Request, plan *archivePlan) error {
	writer := zip.NewWriter(w)
	buffer := make([]byte, 256<<10)
	remaining := archiveMaxBytes
	for _, item := range plan.items {
		if err := r.Context().Err(); err != nil {
			return err
		}
		header := &zip.FileHeader{Name: item.name, Method: zip.Store}
		header.SetMode(0644)
		if item.entry.ModifiedAt != nil {
			if modified, err := time.Parse(time.RFC3339, *item.entry.ModifiedAt); err == nil {
				header.Modified = modified
			}
		}
		if item.entry.Kind == model.KindFolder {
			header.Name += "/"
			header.SetMode(0755 | fs.ModeDir)
		}
		entryWriter, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		if item.entry.Kind == model.KindFile {
			written, err := d.copyArchiveFile(r.Context(), entryWriter, item, remaining, buffer)
			if err != nil {
				return err
			}
			remaining -= written
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	return writer.Close()
}

func (d *RESTDispatcher) copyArchiveFile(ctx context.Context, output io.Writer, item archiveItem, remaining int64, buffer []byte) (int64, error) {
	// Refuse a file that changed between tree enumeration and transfer. This
	// is best effort: WPS does not expose an atomic multi-file snapshot.
	current, err := d.downloads.Metadata(item.path)
	if err != nil {
		return 0, err
	}
	if current.Kind != model.KindFile || current.ID != item.entry.ID ||
		!sameArchiveOptional(current.Size, item.entry.Size) || !sameArchiveOptional(current.Etag, item.entry.Etag) {
		return 0, fmt.Errorf("archive file changed")
	}
	stream, err := d.downloads.OpenPath(ctx, item.path, 0, nil)
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	stopCancel := context.AfterFunc(ctx, func() { stream.Close() })
	defer stopCancel()
	// Resolving the stream may refresh directory metadata. Recheck its
	// identity before consuming bytes from a replacement at the same path.
	d.invalidateTextMetadata()
	current, err = d.downloads.Metadata(item.path)
	if err != nil {
		return 0, err
	}
	if current.Kind != model.KindFile || current.ID != item.entry.ID ||
		!sameArchiveOptional(current.Size, item.entry.Size) || !sameArchiveOptional(current.Etag, item.entry.Etag) {
		return 0, fmt.Errorf("archive file changed")
	}
	if stream.HTTPStatus() != http.StatusOK || stream.ContentRange() != nil {
		return 0, fmt.Errorf("archive received an incomplete object response")
	}
	expected := stream.ContentLength()
	if expected != nil && (*expected < 0 || *expected > remaining || (item.entry.Size != nil && *expected != *item.entry.Size)) {
		return 0, fmt.Errorf("archive object length changed or exceeds limit")
	}
	if expected == nil {
		expected = item.entry.Size
	}
	bound := remaining
	if expected != nil && *expected < bound {
		bound = *expected
	}
	written, err := io.CopyBuffer(output, io.LimitReader(stream, bound+1), buffer)
	if err != nil {
		return written, err
	}
	if err := ctx.Err(); err != nil {
		return written, err
	}
	if written > remaining || (expected != nil && written != *expected) {
		return written, fmt.Errorf("archive object was truncated or exceeds limit")
	}
	return written, nil
}

func sameArchiveOptional[T comparable](a, b *T) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
