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

const previewUnsupportedMessage = "only supported text, image, PDF, audio and video files can be previewed"

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
	if mediaType := previewMediaType(entry.Name); mediaType != "" {
		return sendEntryDownload(w, r, path, true, downloads, limits.chunkSize(), entry, mediaType)
	}
	if !isPreviewableText(entry.Name) && !isPreviewableCode(entry.Name) {
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

// Code is read as bounded inert bytes, never served as an active document.
// Editing and text creation keep their narrower existing extension policy.
func isPreviewableCode(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".markdown", ".js", ".mjs", ".cjs", ".ts", ".jsx", ".tsx", ".go", ".py", ".sh", ".bash", ".css", ".html", ".htm", ".sql", ".rs", ".java", ".c", ".h", ".cpp", ".hpp", ".diff", ".patch":
		return true
	default:
		return false
	}
}

// Active document MIME types such as HTML and SVG are deliberately excluded.
func previewMediaType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".bmp":
		return "image/bmp"
	case ".ico":
		return "image/x-icon"
	case ".pdf":
		return "application/pdf"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".ogv":
		return "video/ogg"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".aac":
		return "audio/aac"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".wav":
		return "audio/wav"
	case ".flac":
		return "audio/flac"
	default:
		return ""
	}
}
