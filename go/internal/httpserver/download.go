// The download stage streams file bodies: metadata first, then the
// slot-managed open_path, then the upstream body copied in chunks straight
// to the client. Range and If-Range handling (206/416) arrives with B802;
// until then a Range header is ignored and the full object is delivered,
// exactly like the interim HEAD behavior of B701.

package httpserver

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// DownloadStorage is the storage surface the streaming GET needs: one
// metadata check and the slot-managed open_path.
type DownloadStorage interface {
	Metadata(path string) (model.RemoteEntry, error)
	OpenPath(ctx context.Context, path string, offset int64, length *int64) (DownloadStream, error)
}

// DownloadStream is the upstream body the GET loop copies. The pointer
// accessors mirror the Python stream's None-or-value fields; storage's
// DownloadStream implementations satisfy it directly.
type DownloadStream interface {
	io.ReadCloser
	HTTPStatus() int
	ContentType() *string
	ContentLength() *int64
	ContentRange() *string
}

// DownloadLimits carries the streaming chunk size. Zero selects the Python
// stream_chunk_size default.
type DownloadLimits struct {
	StreamChunkSize int64
}

// defaultStreamChunkSize mirrors WpsClientConfig.stream_chunk_size.
const defaultStreamChunkSize = 1 << 20

func (l DownloadLimits) chunkSize() int64 {
	if l.StreamChunkSize <= 0 {
		return defaultStreamChunkSize
	}
	return l.StreamChunkSize
}

// sendDownload mirrors _send_download's non-Range GET branch. Both the DAV
// GET route and the REST download route share it, with rest selecting the
// JSON error framing and the Content-Disposition header.
func sendDownload(w http.ResponseWriter, r *http.Request, path string, rest bool, downloads DownloadStorage, chunkSize int64) error {
	entry, err := downloads.Metadata(path)
	if err != nil {
		return err
	}
	if entry.Kind != model.KindFile {
		return model.NewStorageError(model.KindNotFolder, "the requested path is not a file")
	}
	headers := map[string]string{
		"Accept-Ranges":          "bytes",
		"Cache-Control":          "no-store, no-transform",
		"X-Content-Type-Options": "nosniff",
	}
	if entry.Etag != nil && *entry.Etag != "" {
		// Python quotes with f'"{etag.strip(chr(34))}"' and the key must
		// survive Go's canonicalization, so the raw map is used everywhere
		// ETag is written.
		headers["ETag"] = `"` + strings.Trim(*entry.Etag, `"`) + `"`
	}
	if rest {
		headers["Content-Disposition"] = `attachment; filename="` +
			asciiDownloadName(entry.Name) + `"; filename*=UTF-8''` + pythonQuote(entry.Name)
	}
	// The download slot is acquired inside OpenPath; every exit path below
	// releases it together with the upstream body through the deferred
	// Close, mirroring Python's finally: stream.close().
	stream, err := downloads.OpenPath(r.Context(), path, 0, nil)
	if err != nil {
		return err
	}
	defer stream.Close()

	// WPS metadata can lag behind the object the signed URL serves, so the
	// header reflects the object-store length; an unknown or negative one
	// falls back to close framing instead of advertising a stale length.
	streamLength := stream.ContentLength()
	if streamLength != nil && *streamLength >= 0 {
		headers["Content-Length"] = strconv.FormatInt(*streamLength, 10)
	}
	header := w.Header()
	for name, value := range headers {
		header[name] = []string{value}
	}
	header.Set("Content-Type", guessMimeType(entry.Name))
	if _, known := headers["Content-Length"]; known {
		header.Set("Connection", "close")
	} else {
		// Python sends Connection twice here (the header dict plus an
		// explicit send when the length is missing) and frames the body by
		// closing the connection. "Transfer-Encoding: identity" switches
		// net/http off chunked framing; Go removes it from the wire and
		// closes after the response, matching the Python contract.
		header["Connection"] = []string{"close", "close"}
		header["Transfer-Encoding"] = []string{"identity"}
	}
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, chunkSize)
	var written int64
	for {
		// Python checks _client_disconnected before every read; the request
		// context cancels when the client connection drops.
		if r.Context().Err() != nil {
			return nil
		}
		size := len(buf)
		if streamLength != nil {
			remaining := *streamLength - written
			if remaining <= 0 {
				break
			}
			// The +1 lets a lying stream over-read by one byte so the
			// truncation below can detect it.
			if remaining+1 < int64(size) {
				size = int(remaining + 1)
			}
		}
		count, readErr := stream.Read(buf[:size])
		if count == 0 {
			// Python breaks on an empty chunk; a short body leaves the
			// declared Content-Length unfinished and net/http closes the
			// connection, which is the close_connection=True warning path.
			break
		}
		if streamLength != nil {
			if remaining := *streamLength - written; int64(count) > remaining {
				// The stream exceeded its declared length: deliver exactly
				// the promised bytes, then stop.
				w.Write(buf[:remaining])
				return nil
			}
		}
		if _, err := w.Write(buf[:count]); err != nil {
			// Broken pipe, reset, timeout: Python marks the connection for
			// closing and stops writing.
			return nil
		}
		written += int64(count)
		if flusher != nil {
			flusher.Flush()
		}
		if readErr != nil {
			return nil
		}
	}
	return nil
}

// asciiDownloadName mirrors _ascii_download_name: a fixed fallback name for
// clients without filename* support, built from the sanitized extension.
func asciiDownloadName(name string) string {
	suffix := ""
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 && dot != len(name)-1 {
		if candidate := asciiNameSuffix(name[dot+1:]); candidate != "" {
			suffix = "." + candidate
		}
	}
	return "download" + suffix
}

// asciiNameSuffix keeps [A-Za-z0-9_-] like re.sub(r"[^A-Za-z0-9_-]", "", ...)
// and caps the result at 32 characters. Bytes outside ASCII never appear in
// the kept set, so a byte filter matches the rune filter exactly.
func asciiNameSuffix(value string) string {
	var builder strings.Builder
	for index := 0; index < len(value) && builder.Len() < 32; index++ {
		char := value[index]
		if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' ||
			char >= '0' && char <= '9' || char == '_' || char == '-' {
			builder.WriteByte(char)
		}
	}
	return builder.String()
}
