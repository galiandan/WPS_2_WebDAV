package httpserver

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

const propfindContentType = "application/xml; charset=utf-8"

// doPropfind mirrors _do_propfind: a bounded body discard (the prop
// selection is deliberately not parsed — the adapter always answers with
// its fixed property set), Depth validation, the Depth 0/1 walk, and the
// 207 multistatus document with the DAV header.
func (d *DAVDispatcher) doPropfind(w http.ResponseWriter, r *http.Request, davPath string) error {
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	depth, err := propfindDepth(r)
	if err != nil {
		return err
	}
	entries, err := d.webdavEntries(r.Context(), davPath, depth)
	if err != nil {
		return err
	}
	body, err := d.propfindBody(entries)
	if err != nil {
		return err
	}
	writeResponse(w, r, http.StatusMultiStatus, body, propfindContentType, map[string]string{"DAV": davCapabilityHeader}, false)
	return nil
}

// propfindDepth mirrors the Depth header read: default "1", stripped and
// lowercased, and anything but 0/1/infinity answers 400.
func propfindDepth(r *http.Request) (string, error) {
	value := "1"
	if values := r.Header.Values("Depth"); len(values) > 0 {
		value = values[0]
	}
	depth := strings.ToLower(strings.TrimSpace(value))
	switch depth {
	case "0", "1", "infinity":
		return depth, nil
	default:
		return "", errBadRequest("Depth must be 0, 1 or infinity")
	}
}

// propfindEntry is one (href, entry) pair of the multistatus.
type propfindEntry struct {
	href  string
	entry model.RemoteEntry
}

// clientDisconnectedError mirrors Python's _ClientDisconnected: the
// client went away mid-walk, so the response is abandoned entirely and
// the connection closes without an answer.
type clientDisconnectedError struct{}

func (clientDisconnectedError) Error() string { return "client disconnected during PROPFIND" }

// webdavEntries mirrors _webdav_entries as a stack walk: resolve the
// request path once, list the root through the request path (multi-space
// routing), then descend by parent ID — every folder is listed exactly
// once and no deeper node re-resolves from the root. The reversed push
// keeps Python's depth-first pre-order observable. Depth 0 answers only
// the entry; Depth 1 adds its direct children; infinity keeps walking
// until the bounds or the client stop it. A repeated entry ID is an
// upstream integrity failure; the entry and depth bounds raise 507
// exactly like the Python limits; a canceled request context abandons
// the response like Python's _ClientDisconnected.
func (d *DAVDispatcher) webdavEntries(ctx context.Context, path string, depth string) ([]propfindEntry, error) {
	parts, err := storage.SplitRemotePath(path)
	if err != nil {
		return nil, err
	}
	entry, err := d.storage.Metadata(path)
	if err != nil {
		return nil, err
	}
	type frame struct {
		scopePath string
		parts     []string
		entry     model.RemoteEntry
		level     int
	}
	result := make([]propfindEntry, 0)
	visited := make(map[string]struct{})
	stack := []frame{{scopePath: path, parts: parts, entry: entry, level: 0}}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ctx.Err() != nil {
			return nil, clientDisconnectedError{}
		}
		if _, seen := visited[current.entry.ID]; seen {
			return nil, model.NewWpsAPIError("PROPFIND encountered a repeated entry ID", 0, model.WpsCategoryUpstream)
		}
		visited[current.entry.ID] = struct{}{}
		if len(result) >= d.propfind.MaxPropfindEntries {
			return nil, model.NewStorageError(model.KindInsufficientStorage, "PROPFIND exceeds the configured entry limit")
		}
		result = append(result, propfindEntry{href: buildHref(current.parts, current.entry, d.davPrefix), entry: current.entry})
		shouldRecurse := current.entry.Kind == model.KindFolder && (depth == "infinity" || (depth == "1" && current.level == 0))
		if !shouldRecurse {
			continue
		}
		if current.level >= d.propfind.MaxPropfindDepth {
			return nil, model.NewStorageError(model.KindInsufficientStorage, "PROPFIND exceeds the configured depth limit")
		}
		if ctx.Err() != nil {
			return nil, clientDisconnectedError{}
		}
		var children []model.RemoteEntry
		if current.level == 0 {
			// The root listing routes through the request path so the
			// multi-space view can answer virtually.
			children, err = d.storage.ListPath(current.scopePath)
		} else {
			children, err = d.storage.ListChildren(current.scopePath, current.entry)
		}
		if err != nil {
			return nil, err
		}
		for i := len(children) - 1; i >= 0; i-- {
			child := children[i]
			childParts := make([]string, 0, len(current.parts)+1)
			childParts = append(childParts, current.parts...)
			childParts = append(childParts, child.Name)
			childPath, err := storage.JoinRemotePath(childParts, child.Kind == model.KindFolder)
			if err != nil {
				return nil, err
			}
			// The joined path doubles as the child's scope: the multi-space
			// view routes by its first component, and building it keeps the
			// Python join validation (invalid component names) observable.
			stack = append(stack, frame{
				scopePath: childPath,
				parts:     childParts,
				entry:     child,
				level:     current.level + 1,
			})
		}
	}
	return result, nil
}

// buildHref mirrors _href: every path component percent-encoded with
// urllib's quote(part, safe="") semantics, folders ending with a slash.
func buildHref(parts []string, entry model.RemoteEntry, prefix string) string {
	var b strings.Builder
	b.WriteString(prefix)
	if len(parts) == 0 {
		b.WriteString("/")
	} else {
		for _, part := range parts {
			b.WriteString("/")
			b.WriteString(pythonQuote(part))
		}
	}
	href := b.String()
	if entry.Kind == model.KindFolder && !strings.HasSuffix(href, "/") {
		href += "/"
	}
	return href
}

// pythonQuote mirrors urllib.parse.quote with safe="": unreserved ASCII
// survives byte-for-byte, everything else (including the UTF-8 encoding of
// non-ASCII) becomes %XX with uppercase hex.
func pythonQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' || c == '~' {
			b.WriteByte(c)
		} else {
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0F])
		}
	}
	return b.String()
}

const (
	propfindPrefix = `<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`
	propfindSuffix = `</D:multistatus>`
)

// propfindBody mirrors _propfind_body: the fixed multistatus framing plus
// one ElementTree-shaped <D:response> chunk per entry, with the response
// size limit enforced before anything is sent (507 like Python).
func (d *DAVDispatcher) propfindBody(entries []propfindEntry) ([]byte, error) {
	var buf bytes.Buffer
	responseSize := len(propfindPrefix) + len(propfindSuffix)
	buf.WriteString(propfindPrefix)
	for _, item := range entries {
		chunk := propfindResponseChunk(item)
		responseSize += len(chunk)
		if int64(responseSize) > d.limits.MaxResponseBody {
			return nil, model.NewStorageError(model.KindInsufficientStorage, "PROPFIND response exceeds the configured size limit")
		}
		buf.Write(chunk)
	}
	buf.WriteString(propfindSuffix)
	return buf.Bytes(), nil
}

// propfindResponseChunk renders one <D:response> exactly the way Python's
// ElementTree.tostring does: the D namespace redeclared on every chunk,
// empty elements as "<D:tag />", and only &, <, > escaped inside text.
// The property set is fixed: resourcetype, displayname, getcontentlength,
// getcontenttype, and — when present — getetag and getlastmodified.
func propfindResponseChunk(item propfindEntry) []byte {
	entry := item.entry
	var b strings.Builder
	b.WriteString(`<D:response xmlns:D="DAV:"><D:href>`)
	b.WriteString(xmlEscapeText(item.href))
	b.WriteString(`</D:href><D:propstat><D:prop>`)
	// ElementTree serializes an element without children as "<D:tag />".
	if entry.Kind == model.KindFolder {
		b.WriteString(`<D:resourcetype><D:collection /></D:resourcetype>`)
	} else {
		b.WriteString(`<D:resourcetype />`)
	}
	b.WriteString(`<D:displayname>`)
	b.WriteString(xmlEscapeText(entry.Name))
	b.WriteString(`</D:displayname><D:getcontentlength>`)
	size := int64(0)
	if entry.Size != nil {
		size = *entry.Size
	}
	b.WriteString(strconv.FormatInt(size, 10))
	b.WriteString(`</D:getcontentlength><D:getcontenttype>`)
	if entry.Kind == model.KindFolder {
		b.WriteString("httpd/unix-directory")
	} else {
		b.WriteString(guessMimeType(entry.Name))
	}
	b.WriteString(`</D:getcontenttype>`)
	if entry.Etag != nil && *entry.Etag != "" {
		b.WriteString(`<D:getetag>`)
		b.WriteString(xmlEscapeText(`"` + strings.Trim(*entry.Etag, `"`) + `"`))
		b.WriteString(`</D:getetag>`)
	}
	if modified := httpDate(entry.ModifiedAt); modified != "" {
		b.WriteString(`<D:getlastmodified>`)
		b.WriteString(xmlEscapeText(modified))
		b.WriteString(`</D:getlastmodified>`)
	}
	b.WriteString(`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	return []byte(b.String())
}

// xmlEscapeText mirrors ElementTree's text escaping: only &, <, > become
// entities; quotes, control characters, and non-ASCII stay raw UTF-8.
func xmlEscapeText(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// httpDate mirrors _http_date: a float-parsable epoch value formatted with
// email.utils.formatdate(usegmt=True); anything unparseable, non-finite,
// or outside Python's datetime year range returns "" and the property is
// omitted.
func httpDate(value *string) string {
	if value == nil {
		return ""
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(*value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return ""
	}
	seconds := math.Floor(parsed)
	// datetime.fromtimestamp raises OverflowError outside years 1..9999.
	if seconds < -62135596800 || seconds > 253402300799 {
		return ""
	}
	return time.Unix(int64(seconds), 0).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
}
