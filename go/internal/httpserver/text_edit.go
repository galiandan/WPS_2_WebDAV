package httpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

const textEditMaxBytes int64 = 2 << 20

var (
	errTextEditConflict = errors.New("text changed while editing")
	errTextEditTooLarge = errors.New("text exceeds the editor size limit")
)

type textEditorState struct {
	gate chan struct{}
	key  [32]byte
	err  error
}

func (d *RESTDispatcher) editorState() *textEditorState {
	d.textOnce.Do(func() {
		d.textEditor = &textEditorState{gate: make(chan struct{}, 1)}
		_, d.textEditor.err = rand.Read(d.textEditor.key[:])
	})
	return d.textEditor
}

// One editor operation at a time bounds buffered file contents and serializes
// editor saves. Other REST/WebDAV clients and WPS itself remain independent:
// the verified upstream overwrite API has no atomic compare-and-swap, so the
// fresh read immediately before upload is necessarily a best-effort check.
func (s *textEditorState) acquire(ctx context.Context) (func(), error) {
	if s.err != nil {
		return nil, model.NewStorageError(model.KindIOFailure, "text editor is unavailable")
	}
	select {
	case s.gate <- struct{}{}:
		return func() { <-s.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *RESTDispatcher) textIdentity() ([32]byte, error) {
	index := d.searchIndex()
	index.mu.Lock()
	source := index.identitySource
	index.mu.Unlock()
	if source == nil {
		return [32]byte{}, nil
	}
	identity, err := source()
	if err != nil {
		return [32]byte{}, errTextEditConflict
	}
	return identity, nil
}

func (d *RESTDispatcher) invalidateTextMetadata() {
	if cache, ok := d.read.(interface{ InvalidateMetadataCache() }); ok {
		cache.InvalidateMetadataCache()
	}
}

type editableText struct {
	entry    model.RemoteEntry
	body     []byte
	identity [32]byte
	revision string
}

func (s *textEditorState) revision(path string, entry model.RemoteEntry, identity [32]byte, body []byte) string {
	digest := hmac.New(sha256.New, s.key[:])
	// Prefix variable fields with lengths so embedded delimiters cannot alias
	// revisions. The server key also prevents offline guesses of secret-derived
	// identity hashes; neither identities nor internal IDs are exposed.
	for _, part := range [][]byte{[]byte("wps-text-v1"), []byte(path), []byte(entry.ID), identity[:], body} {
		_ = binary.Write(digest, binary.BigEndian, uint64(len(part)))
		_, _ = digest.Write(part)
	}
	return `"` + hex.EncodeToString(digest.Sum(nil)) + `"`
}

func (d *RESTDispatcher) readEditableText(ctx context.Context, path string) (editableText, error) {
	var text editableText
	if err := ctx.Err(); err != nil {
		return text, err
	}
	identity, err := d.textIdentity()
	if err != nil {
		return text, err
	}
	d.invalidateTextMetadata()
	entry, err := d.downloads.Metadata(path)
	if err != nil {
		return text, err
	}
	if entry.Kind != model.KindFile || entry.ID == "" || !isPreviewableText(entry.Name) {
		return text, model.NewStorageError(model.KindUnsupportedOperation, "only existing supported text files can be edited")
	}
	if entry.Size != nil && *entry.Size > textEditMaxBytes {
		return text, errTextEditTooLarge
	}
	stream, err := d.downloads.OpenPath(ctx, path, 0, nil)
	if err != nil {
		return text, err
	}
	defer stream.Close()
	stopCancel := context.AfterFunc(ctx, func() { stream.Close() })
	defer stopCancel()
	if stream.HTTPStatus() != http.StatusOK {
		return text, model.NewWpsAPIError("read complete editor content", 0, model.WpsCategoryUpstream)
	}
	if length := stream.ContentLength(); length != nil && *length > textEditMaxBytes {
		return text, errTextEditTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(stream, textEditMaxBytes+1))
	if ctx.Err() != nil {
		return text, ctx.Err()
	}
	if err != nil {
		return text, err
	}
	if int64(len(body)) > textEditMaxBytes {
		return text, errTextEditTooLarge
	}
	if length := stream.ContentLength(); length != nil && *length >= 0 && *length != int64(len(body)) {
		return text, model.NewWpsAPIError("read complete editor content", 0, model.WpsCategoryUpstream)
	}
	currentIdentity, err := d.textIdentity()
	if err != nil || identity != currentIdentity {
		return text, errTextEditConflict
	}
	// Download resolution can observe a concurrent replacement. Resolve once
	// more without cached listings so an old ID is never paired with new bytes.
	d.invalidateTextMetadata()
	current, err := d.downloads.Metadata(path)
	if err != nil || current.ID != entry.ID || current.Kind != model.KindFile {
		return text, errTextEditConflict
	}
	if entry.Etag != nil && current.Etag != nil && *entry.Etag != *current.Etag {
		return text, errTextEditConflict
	}
	text = editableText{entry: current, body: body, identity: identity}
	text.revision = d.editorState().revision(path, current, identity, body)
	return text, nil
}

func textPath(route RESTRoute) (string, error) {
	path, err := queryPath(route.Query)
	if err != nil {
		return "", err
	}
	return canonicalRemotePath(path)
}

func (d *RESTDispatcher) doTextGet(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	path, err := textPath(route)
	if err != nil {
		return err
	}
	release, err := d.editorState().acquire(r.Context())
	if err != nil {
		return err
	}
	defer release()
	text, err := d.readEditableText(r.Context(), path)
	if err != nil {
		return d.textReadError(w, r, err, false)
	}
	writeResponse(w, r, http.StatusOK, text.body, "application/octet-stream", map[string]string{
		"Etag": text.revision, "X-Editor-Limit": "2097152", "X-Content-Type-Options": "nosniff",
	}, false)
	return nil
}

func (d *RESTDispatcher) textReadError(w http.ResponseWriter, r *http.Request, err error, saving bool) error {
	if errors.Is(err, errTextEditTooLarge) {
		sendError(w, r, http.StatusRequestEntityTooLarge, "text exceeds the 2 MiB editor size limit", true, nil, false)
		return nil
	}
	if errors.Is(err, errTextEditConflict) || errors.Is(err, storage.ErrUploadTargetChanged) || (saving && textTargetError(err)) {
		return d.textFailure(w, r, http.StatusPreconditionFailed, "text_conflict", "file or account changed; reload before saving")
	}
	return err
}

func textTargetError(err error) bool {
	storageErr, ok := model.AsStorageError(err)
	return ok && (storageErr.Kind == model.KindEntryNotFound || storageErr.Kind == model.KindAmbiguousPath || storageErr.Kind == model.KindNotFolder || storageErr.Kind == model.KindUnsupportedOperation)
}

func (d *RESTDispatcher) textFailure(w http.ResponseWriter, r *http.Request, status int, code, message string) error {
	return sendJSON(w, r, status, map[string]string{"error": message, "code": code}, d.limits, nil)
}

func (d *RESTDispatcher) doTextPut(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	path, err := textPath(route)
	if err != nil {
		return err
	}
	revisions := r.Header.Values("If-Match")
	if len(revisions) == 0 {
		return d.textFailure(w, r, http.StatusPreconditionRequired, "text_revision_required", "If-Match from the editor read is required")
	}
	revision := strings.TrimSpace(revisions[0])
	if len(revisions) != 1 || len(revision) != 66 || revision[0] != '"' || revision[65] != '"' {
		return d.textFailure(w, r, http.StatusPreconditionFailed, "text_conflict", "an exact editor revision is required")
	}
	if _, err := hex.DecodeString(revision[1:65]); err != nil {
		return d.textFailure(w, r, http.StatusPreconditionFailed, "text_conflict", "an exact editor revision is required")
	}
	if r.ContentLength > textEditMaxBytes {
		return d.textReadError(w, r, errTextEditTooLarge, true)
	}
	if !checkDeclaredUploadLength(w, r, r.ContentLength, d.maxUploadBytes, true) {
		return nil
	}
	release, err := d.editorState().acquire(r.Context())
	if err != nil {
		return err
	}
	defer release()
	allowed, err := checkLocks(w, r, d.locks, true, path)
	if err != nil || !allowed {
		return err
	}
	stopCancel := context.AfterFunc(r.Context(), func() { r.Body.Close() })
	body, readErr := io.ReadAll(io.LimitReader(r.Body, textEditMaxBytes+1))
	stopCancel()
	if r.Context().Err() != nil {
		return r.Context().Err()
	}
	if readErr != nil || (r.ContentLength >= 0 && r.ContentLength != int64(len(body))) {
		return errBadRequest("incomplete editor request body")
	}
	if int64(len(body)) > textEditMaxBytes {
		return d.textReadError(w, r, errTextEditTooLarge, true)
	}
	if !checkDeclaredUploadLength(w, r, int64(len(body)), d.maxUploadBytes, true) {
		return nil
	}
	if !utf8.Valid(body) {
		return errBadRequest("edited text must be valid UTF-8")
	}
	before, err := d.readEditableText(r.Context(), path)
	if err != nil {
		return d.textReadError(w, r, err, true)
	}
	if !hmac.Equal([]byte(before.revision), []byte(revision)) {
		return d.textReadError(w, r, errTextEditConflict, true)
	}
	allowed, err = checkLocks(w, r, d.locks, true, path)
	if err != nil || !allowed {
		return err
	}
	size := int64(len(body))
	source := &textUploadReader{Reader: bytes.NewReader(body), ctx: r.Context(), identity: before.identity, currentIdentity: d.textIdentity}
	defer d.invalidateTextMetadata()
	_, err = d.uploads.UploadPath(r.Context(), path, source, storage.UploadOptions{
		Size: &size, ContentType: "text/plain; charset=utf-8", Overwrite: true, ExpectedID: before.entry.ID,
	})
	if err != nil {
		if errors.Is(err, storage.ErrUploadTargetChanged) || errors.Is(err, errTextEditConflict) {
			return d.textReadError(w, r, err, true)
		}
		// The legacy WPS writer can have persisted an object before returning
		// an error and cannot atomically cancel after spooling. Never retry an
		// uncertain overwrite or claim it left the original untouched.
		return d.textFailure(w, r, http.StatusBadGateway, "text_save_uncertain", "remote file may have changed; keep your draft and reload to compare before saving again")
	}
	after, err := d.readEditableText(r.Context(), path)
	if err != nil || after.identity != before.identity || !bytes.Equal(after.body, body) {
		return d.textFailure(w, r, http.StatusConflict, "text_save_unverified", "save was submitted but remote content could not be verified; keep your draft and reload to compare")
	}
	return sendJSON(w, r, http.StatusOK, struct {
		Path     string            `json:"path"`
		Entry    model.PublicEntry `json:"entry"`
		Revision string            `json:"revision"`
	}{path, after.entry.Public(), after.revision}, d.limits, map[string]string{"Etag": after.revision})
}

type textUploadReader struct {
	*bytes.Reader
	ctx             context.Context
	identity        [32]byte
	currentIdentity func() ([32]byte, error)
}

func (r *textUploadReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	identity, err := r.currentIdentity()
	if err != nil || identity != r.identity {
		return 0, errTextEditConflict
	}
	return r.Reader.Read(buffer)
}
