package app

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/httpserver"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
	"github.com/galiandan/WPS_2_WebDAV/go/web"
)

type shareAdminStorage struct{ *storage.MultiSpace }

func (s shareAdminStorage) OpenPath(ctx context.Context, path string, offset int64, length *int64) (httpserver.DownloadStream, error) {
	return s.MultiSpace.OpenPath(ctx, path, offset, length)
}
func (a *Application) initShares() error {
	if a.Accounts == nil {
		return nil
	}
	file := a.Config.SharesFile
	if file == "" {
		file = filepath.Join(filepath.Dir(a.Config.WebSettingsDir), "shares.json")
	}
	controller, err := httpserver.NewShareController(httpserver.ShareConfig{
		File: file, ThumbnailSource: a.rest, ResolveOwner: a.resolveShareOwner, StorageFor: a.shareStorage, Binding: a.shareBinding,
	})
	if err != nil {
		return err
	}
	a.Shares = controller
	return nil
}
func (a *Application) resolveShareOwner(id string, version uint64) (auth.Principal, error) {
	if a.Accounts == nil {
		return auth.Principal{}, memberDenied()
	}
	actor, ok := a.Accounts.LookupID(id)
	if !ok || actor.PolicyVersion != version || !actor.Permissions.Read {
		return auth.Principal{}, memberDenied()
	}
	if !actor.IsAdmin() {
		if err := a.validateMember(actor); err != nil {
			return auth.Principal{}, err
		}
	}
	return actor, nil
}
func (a *Application) shareStorage(actor auth.Principal) (httpserver.ShareStorage, error) {
	if _, err := a.resolveShareOwner(actor.ID, actor.PolicyVersion); err != nil {
		return nil, err
	}
	if actor.IsAdmin() {
		return shareAdminStorage{a.Storage}, nil
	}
	view, err := storage.NewScoped(a.Storage, storage.ScopedConfig{
		RootPath: actor.RootPath, RootID: actor.RootID, RootName: "共享目录", Read: true,
		Validate: func() error { return a.validateMember(actor) },
	})
	if err != nil {
		return nil, err
	}
	return memberDownloads{view}, nil
}
func (a *Application) shareBinding(actor auth.Principal, visible string) (string, error) {
	if _, err := a.resolveShareOwner(actor.ID, actor.PolicyVersion); err != nil {
		return "", err
	}
	parts, err := storage.SplitRemotePath(visible)
	if err != nil {
		return "", err
	}
	if !actor.IsAdmin() {
		root, err := storage.SplitRemotePath(actor.RootPath)
		if err != nil {
			return "", err
		}
		parts = append(root, parts...)
	}
	physical, err := storage.JoinRemotePath(parts, false)
	if err != nil {
		return "", err
	}
	return a.rootScopeBinding(physical)
}
func (a *Application) serveShare(w http.ResponseWriter, r *http.Request, id, action string) error {
	if a.Shares == nil {
		return memberDenied()
	}
	if action != "page" {
		return a.Shares.ServePublic(w, r, id, action)
	}
	body := web.SharePage()
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", webContentSecurityPolicy)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
	return nil
}
