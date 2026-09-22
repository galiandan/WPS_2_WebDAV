package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/shares"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

const shareCookieName = "wps_share_grant"
const shareBodyLimit = 16 << 10

type ShareStorage interface {
	RESTReadStorage
	DownloadStorage
}
type ShareConfig struct {
	File            string
	ResolveOwner    func(string, uint64) (auth.Principal, error)
	StorageFor      func(auth.Principal) (ShareStorage, error)
	Binding         func(auth.Principal, string) (string, error)
	ThumbnailSource *RESTDispatcher
}
type ShareController struct {
	store      *shares.Store
	config     ShareConfig
	thumbnails *thumbnailState
}

func NewShareController(config ShareConfig) (*ShareController, error) {
	if config.ResolveOwner == nil || config.StorageFor == nil || config.Binding == nil {
		return nil, errChainConfig("share identity and storage callbacks are required")
	}
	store, err := shares.New(config.File)
	if err != nil {
		return nil, err
	}
	source := config.ThumbnailSource
	if source == nil {
		source = &RESTDispatcher{}
	}
	return &ShareController{store: store, config: config, thumbnails: source.thumbnailState()}, nil
}

type shareMetadata struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	Path             string `json:"path,omitempty"`
	OwnerID          string `json:"owner_id,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
	ExpiresAt        string `json:"expires_at"`
	PasswordRequired bool   `json:"password_required"`
	Revoked          bool   `json:"revoked"`
}

func sharePublic(record shares.Record, management bool) shareMetadata {
	out := shareMetadata{ID: record.ID, Name: record.Spec.Name, Kind: record.Spec.Kind, ExpiresAt: record.ExpiresAt.Format(time.RFC3339Nano), PasswordRequired: record.PasswordRequired(), Revoked: record.Revoked}
	if management {
		out.Path = record.Spec.Path
		out.OwnerID = record.Spec.OwnerID
		out.CreatedAt = record.CreatedAt.Format(time.RFC3339Nano)
	}
	return out
}
func (c *ShareController) currentOwner(principal auth.Principal) (auth.Principal, error) {
	current, err := c.config.ResolveOwner(principal.ID, principal.PolicyVersion)
	if err != nil || current.ID != principal.ID || current.PolicyVersion != principal.PolicyVersion || !current.Permissions.Read {
		return auth.Principal{}, shares.ErrDenied
	}
	return current, nil
}

func (c *ShareController) ServeManagement(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		return model.NewStorageError(model.KindPermissionDenied, "access denied")
	}
	principal, err := c.currentOwner(principal)
	if err != nil {
		return model.NewStorageError(model.KindPermissionDenied, "access denied")
	}
	parts := strings.Split(route.Suffix, "/")
	if len(parts) > 2 {
		return model.NewStorageError(model.KindEntryNotFound, "unknown share route")
	}
	limits := DefaultControlLimits()
	limits.MaxControlBody = shareBodyLimit
	if r.Method == http.MethodGet && len(parts) == 1 {
		if err := discardBody(w, r, limits); err != nil {
			return err
		}
		records := c.store.List(principal.ID, principal.IsAdmin())
		result := make([]shareMetadata, 0, len(records))
		for _, record := range records {
			if !principal.IsAdmin() && record.Spec.PolicyVersion != principal.PolicyVersion {
				continue
			}
			result = append(result, sharePublic(record, true))
		}
		return sendJSON(w, r, 200, map[string]any{"shares": result}, limits, nil)
	}
	if r.Method == http.MethodDelete && len(parts) == 2 {
		if err := discardBody(w, r, limits); err != nil {
			return err
		}
		if _, err := c.currentOwner(principal); err != nil {
			return model.NewStorageError(model.KindPermissionDenied, "access denied")
		}
		if err := c.store.Revoke(parts[1], principal.ID, principal.IsAdmin()); err != nil {
			return shareManagementError(err)
		}
		writeResponse(w, r, 204, nil, contentTypeText, nil, false)
		return nil
	}
	if r.Method != http.MethodPost || len(parts) != 1 {
		return errBadRequest("unsupported share management method")
	}
	payload, err := readJSONBody(w, r, limits)
	if err != nil || payload == nil {
		return err
	}
	for key := range payload {
		if key != "path" && key != "expires_at" && key != "password" {
			return errBadRequest("unknown share field")
		}
	}
	path, ok := payload["path"].(string)
	if !ok {
		return errBadRequest("path is required")
	}
	path, err = canonicalRemotePath(path)
	if err != nil {
		return err
	}
	password := ""
	if value, exists := payload["password"]; exists {
		password, ok = value.(string)
		if !ok {
			return errBadRequest("password must be a string")
		}
	}
	var expires time.Time
	if value, exists := payload["expires_at"]; exists {
		raw, ok := value.(string)
		if !ok {
			return errBadRequest("expires_at must be an RFC3339 timestamp")
		}
		expires, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return errBadRequest("expires_at must be an RFC3339 timestamp")
		}
	}
	spec, err := c.capture(principal, path)
	if err != nil {
		return err
	}
	if r.Context().Err() != nil {
		return r.Context().Err()
	}
	record, token, err := c.store.Create(spec, expires, password)
	if err != nil {
		return shareManagementError(err)
	}
	// Hashing/password work may overlap an owner policy or namespace edit.
	if _, _, err := c.validate(record); err != nil {
		_ = c.store.Revoke(record.ID, principal.ID, false)
		return model.NewStorageError(model.KindPermissionDenied, "share owner or target changed")
	}
	return sendJSON(w, r, 201, map[string]any{"share": sharePublic(record, true), "url": "/share/" + record.ID + "#" + token}, limits, nil)
}

func (c *ShareController) capture(principal auth.Principal, path string) (shares.Spec, error) {
	principal, err := c.currentOwner(principal)
	if err != nil {
		return shares.Spec{}, model.NewStorageError(model.KindPermissionDenied, "access denied")
	}
	before, err := c.config.Binding(principal, path)
	if err != nil {
		return shares.Spec{}, model.NewStorageError(model.KindPermissionDenied, "share target is unavailable")
	}
	backend, err := c.config.StorageFor(principal)
	if err != nil {
		return shares.Spec{}, model.NewStorageError(model.KindPermissionDenied, "share target is unavailable")
	}
	invalidateShareCache(backend)
	entry, err := backend.Metadata(path)
	if err != nil {
		return shares.Spec{}, shareReadError(err)
	}
	if entry.Kind != model.KindFile && entry.Kind != model.KindFolder {
		return shares.Spec{}, model.NewStorageError(model.KindUnsupportedOperation, "only files and folders can be shared")
	}
	after, err := c.config.Binding(principal, path)
	if err != nil || after != before {
		return shares.Spec{}, model.NewStorageError(model.KindAlreadyExists, "storage changed while creating share")
	}
	if _, err := c.currentOwner(principal); err != nil {
		return shares.Spec{}, model.NewStorageError(model.KindPermissionDenied, "share owner changed")
	}
	return shares.Spec{OwnerID: principal.ID, PolicyVersion: principal.PolicyVersion, Path: path, TargetID: entry.ID, Kind: string(entry.Kind), Name: entry.Name, Binding: before}, nil
}

// validate runs before every public operation and every actual storage access.
// Tokens never authorize a generic REST method or a raw storage path.
func (c *ShareController) validate(record shares.Record) (ShareStorage, model.RemoteEntry, error) {
	active, err := c.store.Active(record.ID)
	if err != nil || active.Spec != record.Spec {
		return nil, model.RemoteEntry{}, shares.ErrDenied
	}
	principal, err := c.config.ResolveOwner(record.Spec.OwnerID, record.Spec.PolicyVersion)
	if err != nil || principal.ID != record.Spec.OwnerID || principal.PolicyVersion != record.Spec.PolicyVersion || !principal.Permissions.Read {
		return nil, model.RemoteEntry{}, shares.ErrDenied
	}
	before, err := c.config.Binding(principal, record.Spec.Path)
	if err != nil || before != record.Spec.Binding {
		return nil, model.RemoteEntry{}, shares.ErrDenied
	}
	backend, err := c.config.StorageFor(principal)
	if err != nil {
		return nil, model.RemoteEntry{}, shares.ErrDenied
	}
	invalidateShareCache(backend)
	entry, err := backend.Metadata(record.Spec.Path)
	if err != nil || entry.ID != record.Spec.TargetID || string(entry.Kind) != record.Spec.Kind {
		return nil, model.RemoteEntry{}, shares.ErrDenied
	}
	after, err := c.config.Binding(principal, record.Spec.Path)
	if err != nil || after != before {
		return nil, model.RemoteEntry{}, shares.ErrDenied
	}
	return backend, entry, nil
}
func invalidateShareCache(backend ShareStorage) {
	if cache, ok := backend.(interface{ InvalidateMetadataCache() }); ok {
		cache.InvalidateMetadataCache()
	}
}

// ServePublic handles only the documented read-only public share endpoints.
// The surrounding router must match IDs literally using shares.ValidID.
func (c *ShareController) ServePublic(w http.ResponseWriter, r *http.Request, id, action string) error {
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !shares.ValidID(id) {
		return c.publicDenied(w, r)
	}
	limits := DefaultControlLimits()
	limits.MaxControlBody = shareBodyLimit
	if action == "unlock" {
		if r.Method != http.MethodPost {
			return c.publicDenied(w, r)
		}
		payload, err := readAuthJSONBody(w, r, limits)
		if err != nil || payload == nil {
			if err == nil {
				return nil
			}
			return c.publicDenied(w, r)
		}
		for key := range payload {
			if key != "token" && key != "password" {
				return c.publicDenied(w, r)
			}
		}
		token, ok := payload["token"].(string)
		if !ok {
			return c.publicDenied(w, r)
		}
		password := ""
		if value, exists := payload["password"]; exists {
			password, ok = value.(string)
			if !ok {
				return c.publicDenied(w, r)
			}
		}
		client := r.RemoteAddr
		if host, _, err := net.SplitHostPort(client); err == nil {
			client = host
		}
		record, cookie, expires, err := c.store.Unlock(id, token, password, client, func(record shares.Record) error { _, _, err := c.validate(record); return err })
		if err != nil {
			if errors.Is(err, shares.ErrBusy) {
				sendError(w, r, 503, "share service is busy", true, map[string]string{"Retry-After": "15"}, false)
				return nil
			}
			return c.publicDenied(w, r)
		}
		if err := (&sharedReadView{controller: c, record: record, grant: cookie}).checkGrant(); err != nil {
			return c.publicDenied(w, r)
		}
		http.SetCookie(w, &http.Cookie{Name: shareCookieName, Value: cookie, Path: "/api/share/" + id, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: max(1, int(time.Until(expires).Seconds()))})
		return sendJSON(w, r, 200, map[string]any{"share": sharePublic(record, false), "path": "/", "grant_expires_at": expires.Format(time.RFC3339Nano)}, limits, nil)
	}
	if r.Method != http.MethodGet {
		return c.publicDenied(w, r)
	}
	if action != "info" && action != "entries" && action != "download" && action != "preview" && action != "thumbnail" {
		return c.publicDenied(w, r)
	}
	if err := discardBody(w, r, limits); err != nil {
		return c.publicDenied(w, r)
	}
	cookie, err := r.Cookie(shareCookieName)
	if err != nil {
		return c.publicDenied(w, r)
	}
	record, expires, err := c.store.AuthorizeGrant(id, cookie.Value)
	if err != nil {
		return c.publicDenied(w, r)
	}
	if _, _, err := c.validate(record); err != nil {
		return c.publicDenied(w, r)
	}
	if action == "info" {
		if err := (&sharedReadView{controller: c, record: record, grant: cookie.Value}).checkGrant(); err != nil {
			return c.publicDenied(w, r)
		}
		return sendJSON(w, r, 200, map[string]any{"share": sharePublic(record, false), "path": "/", "grant_expires_at": expires.Format(time.RFC3339Nano)}, limits, nil)
	}
	path, err := queryPath(r.URL.Query())
	if err != nil {
		return c.publicDenied(w, r)
	}
	view := &sharedReadView{controller: c, record: record, grant: cookie.Value}
	if action == "entries" {
		entries, err := view.ListPath(path)
		if err != nil {
			return c.publicReadError(w, r, err)
		}
		public := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			if _, err := storage.JoinRemotePath([]string{entry.Name}, false); err != nil {
				return c.publicReadError(w, r, err)
			}
			public = append(public, map[string]any{"name": entry.Name, "kind": entry.Kind, "size": entry.Size, "modified_at": entry.ModifiedAt, "path": strings.TrimSuffix(path, "/") + "/" + entry.Name})
		}
		return sendJSON(w, r, 200, map[string]any{"path": path, "entries": public}, limits, nil)
	}
	switch action {
	case "download":
		err = sendDownload(w, r, path, true, view, defaultStreamChunkSize)
	case "preview":
		err = sendPreview(w, r, path, view, DownloadLimits{})
	case "thumbnail":
		d := &RESTDispatcher{limits: limits, downloads: view, read: view, taskIdentity: func() ([32]byte, error) {
			if err := view.checkGrant(); err != nil {
				return [32]byte{}, err
			}
			if _, _, err := c.validate(record); err != nil {
				return [32]byte{}, err
			}
			raw, _ := json.Marshal(record.Spec)
			return sha256.Sum256(raw), nil
		}}
		d.thumbnailOnce.Do(func() { d.thumbnails = c.thumbnails })
		err = d.doThumbnail(w, r, path)
	}
	if err != nil {
		if denied := view.checkGrant(); denied != nil {
			return c.publicDenied(w, r)
		}
		return c.publicReadError(w, r, err)
	}
	return nil
}
func (c *ShareController) publicDenied(w http.ResponseWriter, r *http.Request) error {
	sendError(w, r, http.StatusUnauthorized, shares.ErrDenied.Error(), true, nil, false)
	return nil
}
func (c *ShareController) publicReadError(w http.ResponseWriter, r *http.Request, err error) error {
	if errors.Is(err, shares.ErrDenied) {
		return c.publicDenied(w, r)
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return shareReadError(err)
}
func shareReadError(err error) error {
	if domain, ok := model.AsStorageError(err); ok {
		messages := map[model.ErrorKind]string{model.KindEntryNotFound: "entry not found", model.KindNotFolder: "requested entry has the wrong type", model.KindInvalidPath: "invalid share path", model.KindUnsupportedOperation: "preview format is unavailable", model.KindInsufficientStorage: "share operation exceeds the size limit", model.KindServiceBusy: "share service is busy"}
		if message, ok := messages[domain.Kind]; ok {
			return model.NewStorageError(domain.Kind, message)
		}
	}
	return model.NewStorageError(model.KindIOFailure, "share storage request failed")
}
func shareManagementError(err error) error {
	switch {
	case errors.Is(err, shares.ErrDenied):
		return model.NewStorageError(model.KindEntryNotFound, "share not found")
	case errors.Is(err, shares.ErrInvalid):
		return errBadRequest("invalid share: expiry must be in the next 30 days and password must use 4–128 bytes")
	case errors.Is(err, shares.ErrLimit), errors.Is(err, shares.ErrBusy):
		return model.NewStorageError(model.KindServiceBusy, err.Error())
	default:
		return model.NewStorageError(model.KindIOFailure, "share state could not be saved")
	}
}

type sharedReadView struct {
	controller *ShareController
	record     shares.Record
	grant      string
}

func (v *sharedReadView) resolve(path string) (ShareStorage, model.RemoteEntry, string, error) {
	if _, err := v.controller.store.Authorize(v.record.ID, v.grant); err != nil {
		return nil, model.RemoteEntry{}, "", shares.ErrDenied
	}
	parts, err := storage.SplitRemotePath(path)
	if err != nil || len(path) > 4096 {
		return nil, model.RemoteEntry{}, "", model.NewStorageError(model.KindInvalidPath, "invalid share path")
	}
	if v.record.Spec.Kind == "file" && len(parts) != 0 {
		return nil, model.RemoteEntry{}, "", shares.ErrDenied
	}
	backend, root, err := v.controller.validate(v.record)
	if err != nil {
		return nil, model.RemoteEntry{}, "", shares.ErrDenied
	}
	if err := v.checkGrant(); err != nil {
		return nil, model.RemoteEntry{}, "", err
	}
	baseParts, _ := storage.SplitRemotePath(v.record.Spec.Path)
	mapped, err := storage.JoinRemotePath(append(baseParts, parts...), false)
	return backend, root, mapped, err
}
func (v *sharedReadView) Metadata(path string) (model.RemoteEntry, error) {
	backend, root, mapped, err := v.resolve(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if path == "/" {
		root.Name = v.record.Spec.Name
		root.ParentID = nil
		return root, nil
	}
	entry, err := backend.Metadata(mapped)
	if err != nil {
		return model.RemoteEntry{}, shareReadError(err)
	}
	if err := v.checkGrant(); err != nil {
		return model.RemoteEntry{}, err
	}
	entry.ParentID = nil
	return entry, nil
}
func (v *sharedReadView) ListPath(path string) ([]model.RemoteEntry, error) {
	backend, root, mapped, err := v.resolve(path)
	if err != nil {
		return nil, err
	}
	if v.record.Spec.Kind != "folder" {
		return nil, model.NewStorageError(model.KindNotFolder, "not a shared folder")
	}
	selected := root
	if path != "/" {
		selected, err = backend.Metadata(mapped)
		if err != nil {
			return nil, shareReadError(err)
		}
	}
	entries, err := backend.ListPath(mapped)
	if err != nil {
		return nil, shareReadError(err)
	}
	if _, _, err := v.controller.validate(v.record); err != nil {
		return nil, shares.ErrDenied
	}
	if path != "/" {
		current, err := backend.Metadata(mapped)
		if err != nil || current.ID != selected.ID || current.Kind != selected.Kind {
			return nil, shares.ErrDenied
		}
	}
	if err := v.checkGrant(); err != nil {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, model.NewStorageError(model.KindInsufficientStorage, "too many shared entries")
	}
	return entries, nil
}
func (v *sharedReadView) ListChildren(path string, _ model.RemoteEntry) ([]model.RemoteEntry, error) {
	return v.ListPath(path)
}
func (v *sharedReadView) OpenPath(ctx context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	backend, root, mapped, err := v.resolve(path)
	if err != nil {
		return nil, err
	}
	selected := root
	if v.record.Spec.Kind == "folder" {
		selected, err = backend.Metadata(mapped)
		if err != nil {
			return nil, shareReadError(err)
		}
	}
	stream, err := backend.OpenPath(ctx, mapped, offset, length)
	if err != nil {
		return nil, shareReadError(err)
	}
	if _, _, err := v.controller.validate(v.record); err != nil {
		stream.Close()
		return nil, shares.ErrDenied
	}
	if v.record.Spec.Kind == "folder" {
		current, err := backend.Metadata(mapped)
		if err != nil || current.ID != selected.ID || current.Kind != selected.Kind {
			stream.Close()
			return nil, shares.ErrDenied
		}
	}
	if err := v.checkGrant(); err != nil {
		stream.Close()
		return nil, err
	}
	return &sharedGuardStream{DownloadStream: stream, view: v}, nil
}

func (v *sharedReadView) checkGrant() error {
	if _, err := v.controller.store.Authorize(v.record.ID, v.grant); err != nil {
		return shares.ErrDenied
	}
	principal, err := v.controller.config.ResolveOwner(v.record.Spec.OwnerID, v.record.Spec.PolicyVersion)
	if err != nil || principal.ID != v.record.Spec.OwnerID || principal.PolicyVersion != v.record.Spec.PolicyVersion || !principal.Permissions.Read {
		return shares.ErrDenied
	}
	binding, err := v.controller.config.Binding(principal, v.record.Spec.Path)
	if err != nil || binding != v.record.Spec.Binding {
		return shares.ErrDenied
	}
	return nil
}

type sharedGuardStream struct {
	DownloadStream
	view *sharedReadView
}

func (s *sharedGuardStream) Read(body []byte) (int, error) {
	if err := s.view.checkGrant(); err != nil {
		s.Close()
		return 0, err
	}
	n, err := s.DownloadStream.Read(body)
	if accessErr := s.view.checkGrant(); accessErr != nil {
		s.Close()
		return 0, accessErr
	}
	return n, err
}
