package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/accounts"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/httpserver"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

const maxMemberServices = 8

type memberService struct {
	principal auth.Principal
	rest      *httpserver.RESTDispatcher
	dav       *httpserver.DAVDispatcher
	used      time.Time
}

type memberDownloads struct{ *storage.Scoped }

func (d memberDownloads) OpenPath(ctx context.Context, path string, offset int64, length *int64) (httpserver.DownloadStream, error) {
	return d.Scoped.OpenPath(ctx, path, offset, length)
}

func (a *Application) initAccounts() error {
	if a.Sessions == nil {
		return nil
	}
	file := a.Config.UsersFile
	if file == "" {
		file = filepath.Join(filepath.Dir(a.Config.WebSettingsDir), "users.json")
	}
	store, err := accounts.New(file, adapterAuthConfig(a.Config).Credentials)
	if err != nil {
		return err
	}
	hub, err := auth.NewAccountStores(a.Sessions, store)
	if err != nil {
		return err
	}
	a.Accounts, a.AccountHub = store, hub
	return nil
}
func memberDenied() error { return model.NewStorageError(model.KindPermissionDenied, "access denied") }

func (a *Application) validateMember(expected auth.Principal) error {
	if a.Accounts == nil {
		return memberDenied()
	}
	current, ok := a.Accounts.LookupID(expected.ID)
	if !ok || current != expected || current.IsAdmin() {
		return memberDenied()
	}
	binding, err := a.rootScopeBinding(expected.RootPath)
	if err != nil || expected.RootBinding == "" || binding != expected.RootBinding {
		return memberDenied()
	}
	return nil
}

func (a *Application) memberIdentity(principal auth.Principal) ([32]byte, error) {
	if err := a.validateMember(principal); err != nil {
		return [32]byte{}, err
	}
	base, err := a.taskIdentity()
	if err != nil {
		return [32]byte{}, err
	}
	encoded, err := json.Marshal(struct {
		Account auth.Principal
		Backing [32]byte
	}{principal, base})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func (a *Application) member(principal auth.Principal) (*memberService, error) {
	if err := a.validateMember(principal); err != nil {
		return nil, err
	}
	a.memberMu.Lock()
	defer a.memberMu.Unlock()
	if a.memberServices == nil {
		a.memberServices = make(map[string]*memberService)
	}
	if service := a.memberServices[principal.ID]; service != nil {
		if service.principal == principal {
			service.used = time.Now()
			return service, nil
		}
		service.rest.StopMemberServices()
		delete(a.memberServices, principal.ID)
	}
	if len(a.memberServices) >= maxMemberServices {
		oldest := ""
		for id, service := range a.memberServices {
			if oldest == "" || service.used.Before(a.memberServices[oldest].used) {
				oldest = id
			}
		}
		a.memberServices[oldest].rest.StopMemberServices()
		delete(a.memberServices, oldest)
	}
	scoped, err := storage.NewScoped(a.Storage, storage.ScopedConfig{
		RootPath: principal.RootPath, RootID: principal.RootID, RootName: "我的文件",
		Read: principal.Permissions.Read, Upload: principal.Permissions.Upload, Delete: principal.Permissions.Delete,
		Validate: func() error { return a.validateMember(principal) },
	})
	if err != nil {
		return nil, err
	}
	downloads := memberDownloads{scoped}
	rest, err := a.rest.NewMemberDispatcher(scoped, downloads, func() ([32]byte, error) { return a.memberIdentity(principal) }, principal.ID, principal.PolicyVersion, "我的文件")
	if err != nil {
		return nil, err
	}
	dav, err := a.rest.NewMemberDAV(scoped, downloads, scoped, scoped, httpserver.DAVLimits{MaxPropfindEntries: a.Config.MaxPropfindEntries, MaxPropfindDepth: a.Config.MaxPropfindDepth}, a.Config.DAVPrefix)
	if err != nil {
		return nil, err
	}
	service := &memberService{principal: principal, rest: rest, dav: dav, used: time.Now()}
	a.memberServices[principal.ID] = service
	return service, nil
}
func (a *Application) resolveTaskOwner(id string, version uint64) (*httpserver.RESTDispatcher, error) {
	if a.Accounts == nil {
		return nil, memberDenied()
	}
	principal, ok := a.Accounts.LookupID(id)
	if !ok || principal.PolicyVersion != version || principal.IsAdmin() {
		return nil, memberDenied()
	}
	service, err := a.member(principal)
	if err != nil {
		return nil, err
	}
	return service.rest, nil
}

func (a *Application) serveScopedREST(w http.ResponseWriter, r *http.Request, route httpserver.RESTRoute) error {
	// Authentication routes resolve their own principal via AccountStores,
	// including anonymous login/challenge endpoints.
	if route.Suffix == "auth" || strings.HasPrefix(route.Suffix, "auth/") {
		return a.rest.ServeREST(w, r, route)
	}
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		if a.Accounts == nil {
			return a.rest.ServeREST(w, r, route)
		}
		return memberDenied()
	}
	if principal.IsAdmin() {
		return a.rest.ServeREST(w, r, route)
	}
	allowed := false
	switch route.Suffix {
	case "entries", "list", "metadata", "download", "preview", "thumbnail", "archive", "batch", "folders", "folder", "upload", "files", "delete", "text", "search", "search/refresh", "tasks":
		allowed = true
	case "settings", "status":
		allowed = r.Method == http.MethodGet
	default:
		allowed = strings.HasPrefix(route.Suffix, "tasks/")
	}
	if !allowed {
		return memberDenied()
	}
	service, err := a.member(principal)
	if err != nil {
		return err
	}
	return service.rest.ServeREST(w, r, route)
}
func (a *Application) serveScopedDAV(w http.ResponseWriter, r *http.Request, path string) error {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		if a.Accounts == nil {
			return a.serveDAVWithSearch(w, r, path)
		}
		return memberDenied()
	}
	if principal.IsAdmin() {
		return a.serveDAVWithSearch(w, r, path)
	}
	if (r.Method == "LOCK" || r.Method == "UNLOCK") && !principal.Permissions.Upload && !principal.Permissions.Delete {
		return memberDenied()
	}
	service, err := a.member(principal)
	if err != nil {
		return err
	}
	switch r.Method {
	case "PUT", "MKCOL", "DELETE", "MOVE", "COPY":
		a.invalidateAllSearch()
		defer a.invalidateAllSearch()
	}
	return service.dav.ServeDAV(w, r, path)
}
func (a *Application) stopMemberServices() {
	a.memberMu.Lock()
	defer a.memberMu.Unlock()
	for _, service := range a.memberServices {
		service.rest.StopMemberServices()
	}
	a.memberServices = nil
}

func (a *Application) invalidateAllSearch() {
	if a.rest != nil {
		a.rest.InvalidateSearch()
	}
	a.memberMu.Lock()
	services := make([]*memberService, 0, len(a.memberServices))
	for _, service := range a.memberServices {
		services = append(services, service)
	}
	a.memberMu.Unlock()
	for _, service := range services {
		service.rest.InvalidateSearch()
	}
}

// WPS root IDs such as "0" are reused by different spaces. Bind the assigned
// directory to its actual group and API realm as well as the separately checked
// root ID. Cookie refresh does not change this stable resource namespace.
func (a *Application) rootScopeBinding(path string) (string, error) {
	namespace, err := a.Storage.ScopeBinding(path)
	if err != nil {
		return "", err
	}
	mode := a.Config.Mode
	if a.State != nil {
		mode, err = a.State.Mode()
		if err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal([]string{namespace, mode, a.Config.BaseURL, a.Config.AccountBaseURL, a.Config.ObjectSuffix})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}
