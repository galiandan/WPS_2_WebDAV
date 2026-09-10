package httpserver

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

const previewUnsupportedMessage = "only .txt files can be previewed"

// sendPreview serves a bounded, non-download text response for the browser
// preview. The upstream stream is still slot-managed by OpenPath, while
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
	}
	if truncated {
		extra["X-Preview-Truncated"] = "true"
	}
	writeResponse(w, r, http.StatusOK, body, "text/plain; charset=utf-8", extra, false)
	return nil
}

func isPreviewableText(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".txt")
}
