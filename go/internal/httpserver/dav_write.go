// The WebDAV write methods: MKCOL, DELETE, MOVE, COPY, and the LOCK/UNLOCK
// protocol, ported from server.py's _do_webdav_* handlers. Every mutation
// checks the process-local lock store for the source and the exact
// destination before touching WPS, and COPY never deletes an existing
// destination.

package httpserver

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

// maxLockBodyBytes mirrors _read_lock_owner's 64 KiB owner-body bound.
const maxLockBodyBytes = 64 * 1024

// maxLockOwnerRunes mirrors the [:512] owner truncation.
const maxLockOwnerRunes = 512

// lockTimeoutPattern mirrors _lock_timeout's Second-N search.
var lockTimeoutPattern = regexp.MustCompile(`(?i)second-(\d+)`)

// canonicalRemotePath mirrors _canonical_path: join(split(path)) in the
// decoded business-path space. Split failures propagate as domain errors.
func canonicalRemotePath(path string) (string, error) {
	parts, err := storage.SplitRemotePath(path)
	if err != nil {
		return "", err
	}
	return storage.JoinRemotePath(parts, false)
}

// storageEntryNotFound reports the one storage error the COPY/MOVE/LOCK
// handlers tolerate.
func storageEntryNotFound(err error) bool {
	storageErr, ok := model.AsStorageError(err)
	return ok && storageErr.Kind == model.KindEntryNotFound
}

// checkLocks mirrors _check_locks: every named path must be releasable by
// the tokens in If/Lock-Token; the first denial answers 423 (JSON framing
// for REST routes) and reports false. Canonicalization failures propagate
// like the Python join(split(path)) raise.
func checkLocks(w http.ResponseWriter, r *http.Request, store *DavLockStore, rest bool, paths ...string) (bool, error) {
	tokens := TokensFromHeaders(r.Header.Get("If"), r.Header.Get("Lock-Token"))
	for _, path := range paths {
		canonical, err := canonicalRemotePath(path)
		if err != nil {
			return false, err
		}
		if !store.Allows(canonical, tokens) {
			sendError(w, r, http.StatusLocked, "resource is locked", rest, nil, false)
			return false, nil
		}
	}
	return true, nil
}

// headerValue mirrors self.headers.get(name, default): the default applies
// only when the header is absent; a present-but-empty value stays empty and
// fails validation like the reference.
func headerValue(r *http.Request, name string, defaultValue string) string {
	if values, ok := r.Header[name]; ok && len(values) > 0 {
		return values[0]
	}
	return defaultValue
}

// overwriteHeader mirrors _overwrite_header: T (default) or F, case- and
// whitespace-tolerant, a bare ValueError otherwise.
func overwriteHeader(r *http.Request) (bool, error) {
	value := strings.TrimSpace(strings.ToUpper(headerValue(r, "Overwrite", "T")))
	switch value {
	case "T":
		return true, nil
	case "F":
		return false, nil
	}
	return false, errBadRequest("Overwrite must be T or F")
}

// destinationDavPath mirrors _destination_dav_path: no credentials, query,
// or fragment; an absolute destination must point at this adapter's host
// and port; the business path is the raw remainder below the DAV prefix,
// which every consumer decodes exactly once.
func (d *DAVDispatcher) destinationDavPath(r *http.Request) (string, error) {
	destination := r.Header.Get("Destination")
	if destination == "" {
		return "", model.NewStorageError(model.KindInvalidPath, "Destination header is required")
	}
	parsed, err := url.Parse(destination)
	if err != nil {
		return "", model.NewStorageError(model.KindInvalidPath, "Destination must point inside the WebDAV path")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", model.NewStorageError(model.KindInvalidPath, "Destination must not contain credentials, query, or fragment")
	}
	if parsed.Host != "" {
		destinationHost := strings.ToLower(parsed.Hostname())
		requestParts, err := url.Parse("//" + r.Host)
		if err != nil {
			return "", model.NewStorageError(model.KindInvalidPath, "Destination host or port is invalid")
		}
		if destinationHost == "" || requestParts.Hostname() == "" ||
			destinationHost != strings.ToLower(requestParts.Hostname()) ||
			parsed.Port() != requestParts.Port() {
			return "", model.NewStorageError(model.KindInvalidPath, "Destination must point to this adapter")
		}
	}
	destinationPath := parsed.EscapedPath()
	prefix := d.davPrefix
	if destinationPath == prefix {
		return "/", nil
	}
	if !strings.HasPrefix(destinationPath, prefix+"/") {
		return "", model.NewStorageError(model.KindInvalidPath, "Destination must point inside the WebDAV path")
	}
	remainder := destinationPath[len(prefix):]
	if remainder == "" {
		return "/", nil
	}
	// Python decodes the destination exactly once inside split_remote_path;
	// the Go handler performs that single decode so every consumer shares
	// the same business path.
	return unquotePercent(remainder), nil
}

// doDavMkcol mirrors _do_webdav_mkcol: the lock check runs before the body
// is discarded, then the folder is created and answered with its JSON and
// Location header.
func (d *DAVDispatcher) doDavMkcol(w http.ResponseWriter, r *http.Request, davPath string) error {
	allowed, err := checkLocks(w, r, d.locks, false, davPath)
	if err != nil {
		return err
	}
	if !allowed {
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		return nil
	}
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	entry, err := d.mutations.CreateFolderPath(davPath)
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

// doDavDelete mirrors do_DELETE's DAV branch: the body is discarded first,
// then the lock check, then the delete answers 204.
func (d *DAVDispatcher) doDavDelete(w http.ResponseWriter, r *http.Request, davPath string) error {
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	allowed, err := checkLocks(w, r, d.locks, false, davPath)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	if err := d.mutations.DeletePath(davPath); err != nil {
		return err
	}
	writeResponse(w, r, http.StatusNoContent, nil, contentTypeText, nil, false)
	return nil
}

// doDavMove mirrors _do_webdav_move: both endpoints are lock-checked, an
// existing destination never gets overwritten (412 with Overwrite F, 501
// otherwise), and same-path moves fall through to storage's own semantics.
func (d *DAVDispatcher) doDavMove(w http.ResponseWriter, r *http.Request, davPath string) error {
	destination, err := d.destinationDavPath(r)
	if err != nil {
		return err
	}
	allowed, err := checkLocks(w, r, d.locks, false, davPath, destination)
	if err != nil {
		return err
	}
	if !allowed {
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		return nil
	}
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	overwrite, err := overwriteHeader(r)
	if err != nil {
		return err
	}
	canonical, err := canonicalRemotePath(davPath)
	if err != nil {
		return err
	}
	destinationCanonical, err := canonicalRemotePath(destination)
	if err != nil {
		return err
	}
	samePath := canonical == destinationCanonical
	destinationExists := false
	if !samePath {
		if _, err := d.storage.Metadata(destination); err == nil {
			destinationExists = true
		} else if !storageEntryNotFound(err) {
			return err
		}
	}
	if destinationExists {
		if !overwrite {
			sendError(w, r, http.StatusPreconditionFailed, "destination already exists", false, nil, false)
			return nil
		}
		sendError(w, r, http.StatusNotImplemented, "MOVE overwrite is disabled because WPS move is not atomic", false, nil, false)
		return nil
	}
	entry, err := d.mutations.MovePath(davPath, destination)
	if err != nil {
		return err
	}
	parts, err := storage.SplitRemotePath(destination)
	if err != nil {
		return err
	}
	extra := map[string]string{"Location": buildHref(parts, entry, d.davPrefix)}
	if destinationExists {
		writeResponse(w, r, http.StatusNoContent, nil, contentTypeText, extra, false)
		return nil
	}
	return sendJSON(w, r, http.StatusCreated, entry.Public(), d.limits, extra)
}

// doDavCopy mirrors _do_webdav_copy: depth and overwrite gates run before
// the lock check, an existing destination is refused (412/501) and never
// deleted, and the relay's already-exists race answers 412 again.
func (d *DAVDispatcher) doDavCopy(w http.ResponseWriter, r *http.Request, davPath string) error {
	destination, err := d.destinationDavPath(r)
	if err != nil {
		return err
	}
	depth := strings.TrimSpace(strings.ToLower(headerValue(r, "Depth", "infinity")))
	if depth != "0" && depth != "1" && depth != "infinity" {
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		sendError(w, r, http.StatusBadRequest, "Depth must be 0, 1 or infinity", false, nil, false)
		return nil
	}
	overwrite, err := overwriteHeader(r)
	if err != nil {
		return err
	}
	allowed, err := checkLocks(w, r, d.locks, false, davPath, destination)
	if err != nil {
		return err
	}
	if !allowed {
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		return nil
	}
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}

	destinationExists := true
	if _, err := d.storage.Metadata(destination); err != nil {
		if !storageEntryNotFound(err) {
			return err
		}
		destinationExists = false
	}
	if destinationExists && !overwrite {
		sendError(w, r, http.StatusPreconditionFailed, "destination already exists", false, nil, false)
		return nil
	}
	if destinationExists {
		sendError(w, r, http.StatusNotImplemented, "COPY overwrite is disabled because the relay is not atomic", false, nil, false)
		return nil
	}
	entry, err := d.mutations.CopyPath(r.Context(), davPath, destination, storage.CopyOptions{Depth: depth, Overwrite: overwrite})
	if err != nil {
		if alreadyExists(err) && !overwrite {
			sendError(w, r, http.StatusPreconditionFailed, "destination already exists", false, nil, false)
			return nil
		}
		return err
	}
	parts, err := storage.SplitRemotePath(destination)
	if err != nil {
		return err
	}
	extra := map[string]string{"Location": buildHref(parts, entry, d.davPrefix)}
	if destinationExists {
		writeResponse(w, r, http.StatusNoContent, nil, contentTypeText, extra, false)
		return nil
	}
	return sendJSON(w, r, http.StatusCreated, entry.Public(), d.limits, extra)
}

// alreadyExists reports the storage conflict the COPY race branch tolerates.
func alreadyExists(err error) bool {
	storageErr, ok := model.AsStorageError(err)
	return ok && storageErr.Kind == model.KindAlreadyExists
}

// lockDepthHeader mirrors _lock_depth.
func lockDepthHeader(r *http.Request) (string, error) {
	value := strings.TrimSpace(strings.ToLower(headerValue(r, "Depth", "infinity")))
	if value != "0" && value != "infinity" {
		return "", errBadRequest("LOCK Depth must be 0 or infinity")
	}
	return value, nil
}

// lockTimeoutHeader mirrors _lock_timeout: Infinite or Second-N, clamped
// into [1, max_timeout]. The digit count is unbounded like Python's int().
func lockTimeoutHeader(r *http.Request, store *DavLockStore) (int, error) {
	value := headerValue(r, "Timeout", "Second-3600")
	if strings.TrimSpace(strings.ToLower(value)) == "infinite" {
		return store.maxTimeout, nil
	}
	match := lockTimeoutPattern.FindStringSubmatch(value)
	if match == nil {
		return 0, errBadRequest("Timeout must be Second-N or Infinite")
	}
	parsed, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		// Python's int() is unbounded; an overflow clamps to the maximum
		// exactly like every value above the limit.
		return store.maxTimeout, nil
	}
	timeout := int(parsed)
	if timeout < 1 {
		timeout = 1
	}
	if timeout > store.maxTimeout {
		timeout = store.maxTimeout
	}
	return timeout, nil
}

// readLockOwner mirrors _read_lock_owner: a bounded exact read, DTD/entity
// refusal, strict XML parsing, and the first owner element's whitespace-
// collapsed text truncated to 512 runes.
func readLockOwner(w http.ResponseWriter, r *http.Request) (string, error) {
	length, err := contentLength(w, r, false)
	if err != nil || length == nil {
		return "", err
	}
	if *length == 0 {
		return "", nil
	}
	if *length > maxLockBodyBytes {
		return "", errRequestBodyTooLarge()
	}
	body := make([]byte, *length)
	if _, err := io.ReadFull(r.Body, body); err != nil {
		return "", errBadRequestClose("request body is shorter than Content-Length")
	}
	return lockOwnerFromBody(body)
}

// lockOwnerFromBody extracts the first owner element's text with Python's
// ElementTree strictness: the document must be well-formed XML with exactly
// one root and no junk outside it.
func lockOwnerFromBody(body []byte) (string, error) {
	if !utf8.Valid(body) {
		return "", errBadRequest("LOCK request body must be valid XML")
	}
	lower := strings.ToLower(string(body))
	if strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<!entity") {
		return "", errBadRequest("LOCK request body must not declare XML entities")
	}
	owner, err := firstXMLElementText(body, "owner")
	if err != nil {
		return "", errBadRequest("LOCK request body must be valid XML")
	}
	fields := strings.Fields(owner)
	owner = strings.Join(fields, " ")
	runes := []rune(owner)
	if len(runes) > maxLockOwnerRunes {
		owner = string(runes[:maxLockOwnerRunes])
	}
	return owner, nil
}

// RemainingSeconds reports max(1, int(expires_at - now)) for the lock
// response body, driven by the store's monotonic clock.
func (s *DavLockStore) RemainingSeconds(active ActiveLock) int {
	remaining := active.ExpiresAt.Sub(s.now())
	seconds := int64(remaining / time.Second)
	if seconds < 1 {
		return 1
	}
	return int(seconds)
}

// hrefPath mirrors _href_path: every component quoted, no trailing slash.
func hrefPath(path string, prefix string) string {
	parts, err := storage.SplitRemotePath(path)
	if err != nil {
		return prefix + "/"
	}
	var builder strings.Builder
	builder.WriteString(prefix)
	if len(parts) == 0 {
		builder.WriteString("/")
	} else {
		for _, part := range parts {
			builder.WriteString("/")
			builder.WriteString(pythonQuote(part))
		}
	}
	return builder.String()
}

// lockResponseBody mirrors _lock_body's ElementTree serialization: the
// registered D: prefix, empty elements as "<tag />", and the declaration
// line. The timeout carries the remaining seconds the caller computed from
// the store's monotonic clock.
func lockResponseBody(active ActiveLock, remainingSeconds int, lockrootHref string) []byte {
	depth := "0"
	if active.Depth == "infinity" {
		depth = "Infinity"
	}
	var builder strings.Builder
	builder.WriteString("<?xml version='1.0' encoding='utf-8'?>\n")
	builder.WriteString(`<D:prop xmlns:D="DAV:"><D:lockdiscovery><D:activelock>`)
	builder.WriteString(`<D:locktype><D:write /></D:locktype>`)
	builder.WriteString(`<D:lockscope><D:exclusive /></D:lockscope>`)
	builder.WriteString("<D:depth>" + depth + "</D:depth>")
	if active.Owner != "" {
		builder.WriteString("<D:owner>" + xmlText(active.Owner) + "</D:owner>")
	} else {
		builder.WriteString("<D:owner />")
	}
	builder.WriteString("<D:timeout>Second-" + strconv.Itoa(remainingSeconds) + "</D:timeout>")
	builder.WriteString(`<D:locktoken><D:href>` + xmlText(active.Token) + `</D:href></D:locktoken>`)
	builder.WriteString(`<D:lockroot><D:href>` + xmlText(lockrootHref) + `</D:href></D:lockroot>`)
	builder.WriteString(`</D:activelock></D:lockdiscovery></D:prop>`)
	return []byte(builder.String())
}

// xmlText escapes one text node the way ElementTree does: ampersand, less-
// than, and greater-than become entities; everything else stays raw.
func xmlText(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(value)
}

// firstXMLElementText walks the document with ElementTree.fromstring's
// strictness — exactly one root, every start closed, no non-whitespace
// content outside the root, no unbound prefixes — and returns the full
// itertext() of the first element whose local name matches, whatever its
// namespace. Comments and processing instructions never contribute text.
func firstXMLElementText(body []byte, local string) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	sawRoot := false
	capturing := false
	captureDone := false
	captureDepth := 0
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			if typed.Name.Space == "" && strings.Contains(typed.Name.Local, ":") {
				// Python's parser refuses unbound prefixes outright.
				return "", errors.New("unbound prefix")
			}
			if sawRoot && depth == 0 {
				return "", errors.New("second root element")
			}
			depth++
			sawRoot = true
			// Only the first matching element wins, like root.iter() with
			// next(); later matches are ignored.
			if !captureDone && !capturing && typed.Name.Local == local {
				capturing = true
				captureDepth = depth
			}
		case xml.EndElement:
			if capturing && depth == captureDepth {
				capturing = false
				captureDone = true
			}
			depth--
			if depth < 0 {
				return "", errors.New("unbalanced end element")
			}
		case xml.CharData:
			content := string(typed)
			for index := 0; index < len(content); index++ {
				if content[index] < 0x20 && content[index] != '\t' && content[index] != '\n' && content[index] != '\r' {
					return "", errors.New("invalid character data")
				}
			}
			if capturing {
				text.WriteString(content)
				continue
			}
			if strings.TrimSpace(content) != "" && (depth == 0 || !sawRoot) {
				return "", errors.New("content outside the root element")
			}
		case xml.Comment, xml.ProcInst, xml.Directive:
			// ElementTree drops comments and processing instructions and
			// the byte-level pre-check already removed DTD declarations.
		default:
			return "", errors.New("unexpected XML token")
		}
	}
	if !sawRoot || depth != 0 {
		return "", errors.New("incomplete XML document")
	}
	return text.String(), nil
}

// sendLockResponse mirrors _send_lock_response.
func (d *DAVDispatcher) sendLockResponse(w http.ResponseWriter, r *http.Request, status int, active ActiveLock) error {
	body := lockResponseBody(active, d.locks.RemainingSeconds(active), hrefPath(active.Path, d.davPrefix))
	extra := map[string]string{
		"DAV":        "1,2",
		"Lock-Token": "<" + active.Token + ">",
	}
	writeResponse(w, r, status, body, "application/xml; charset=utf-8", extra, false)
	return nil
}

// doDavLock mirrors _do_lock: exactly one If/Lock-Token token turns the
// request into a refresh; otherwise the resource metadata decides between
// 200 and 201 for a new lock.
func (d *DAVDispatcher) doDavLock(w http.ResponseWriter, r *http.Request, davPath string) error {
	canonical, err := canonicalRemotePath(davPath)
	if err != nil {
		return err
	}
	tokens := TokensFromHeaders(r.Header.Get("If"), r.Header.Get("Lock-Token"))
	if len(tokens) > 1 {
		return errBadRequest("LOCK request contains multiple lock tokens")
	}
	var refreshToken string
	for token := range tokens {
		refreshToken = token
	}
	depth, err := lockDepthHeader(r)
	if err != nil {
		return err
	}
	timeout, err := lockTimeoutHeader(r, d.locks)
	if err != nil {
		return err
	}
	owner, err := readLockOwner(w, r)
	if err != nil {
		return err
	}

	if refreshToken != "" {
		if !d.locks.Allows(canonical, tokens) {
			sendError(w, r, http.StatusLocked, "resource is locked", false, nil, false)
			return nil
		}
		active, err := d.locks.Acquire(canonical, depth, owner, timeout, refreshToken)
		if err != nil {
			if errors.Is(err, errLockTokenInvalid) {
				sendError(w, r, http.StatusConflict, "lock token is invalid", false, nil, false)
				return nil
			}
			return err
		}
		return d.sendLockResponse(w, r, http.StatusOK, active)
	}

	if !d.locks.Allows(canonical, tokens) {
		sendError(w, r, http.StatusLocked, "resource is locked", false, nil, false)
		return nil
	}
	existed := true
	if _, err := d.storage.Metadata(davPath); err != nil {
		if !storageEntryNotFound(err) {
			return err
		}
		existed = false
	}
	active, err := d.locks.Acquire(canonical, depth, owner, timeout, "")
	if err != nil {
		if errors.Is(err, errLockConflict) {
			sendError(w, r, http.StatusLocked, "resource is locked", false, nil, false)
			return nil
		}
		return err
	}
	status := http.StatusOK
	if !existed {
		status = http.StatusCreated
	}
	return d.sendLockResponse(w, r, status, active)
}

// doDavUnlock mirrors _do_unlock: the body is discarded first, exactly one
// Lock-Token token is required, and the removal answers 204.
func (d *DAVDispatcher) doDavUnlock(w http.ResponseWriter, r *http.Request, davPath string) error {
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	tokens := TokensFromHeaders(r.Header.Get("Lock-Token"))
	if len(tokens) != 1 {
		return errBadRequest("Lock-Token header is required")
	}
	var token string
	for value := range tokens {
		token = value
	}
	canonical, err := canonicalRemotePath(davPath)
	if err != nil {
		return err
	}
	if err := d.locks.Unlock(canonical, token); err != nil {
		if errors.Is(err, errLockTokenInvalid) {
			sendError(w, r, http.StatusConflict, "lock token is invalid", false, nil, false)
			return nil
		}
		return err
	}
	writeResponse(w, r, http.StatusNoContent, nil, contentTypeText, nil, false)
	return nil
}
