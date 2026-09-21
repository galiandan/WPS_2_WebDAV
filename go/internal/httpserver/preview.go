package httpserver

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

const previewUnsupportedMessage = "only supported text files can be previewed"

// sendPreview serves bounded raw bytes for browser-side text decoding.
// The upstream stream is still slot-managed by OpenPath, while
// LimitReader guarantees that a large file cannot become a large heap read.
func sendPreview(w http.ResponseWriter, r *http.Request, path string, downloads DownloadStorage, limits DownloadLimits) error {
	entry, err := downloads.Metadata(path)
	if err != nil {
		return err
	}
	if entry.Kind != model.KindFile {
		return model.NewStorageError(model.KindNotFolder, "the requested path is not a file")
	}
	if !isPreviewableText(entry.Name) {
		return model.NewStorageError(model.KindUnsupportedOperation, previewUnsupportedMessage)
	}

	stream, err := downloads.OpenPath(r.Context(), path, 0, nil)
	if err != nil {
		return err
	}
	defer stream.Close()
	stopCancel := context.AfterFunc(r.Context(), func() { stream.Close() })
	defer stopCancel()

	limit := limits.previewLimit()
	body, err := io.ReadAll(io.LimitReader(stream, limit+1))
	if err != nil {
		return err
	}
	truncated := int64(len(body)) > limit
	if truncated {
		body = body[:limit]
	}
	extra := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Preview-Truncated":    "false",
		"X-Preview-Limit":        strconv.FormatInt(limit, 10),
	}
	if truncated {
		extra["X-Preview-Truncated"] = "true"
	}
	writeResponse(w, r, http.StatusOK, body, "application/octet-stream", extra, false)
	return nil
}

func isPreviewableText(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".log", ".md", ".csv", ".json", ".xml", ".yaml", ".yml", ".ini", ".conf", ".toml":
		return true
	default:
		return false
	}
}
