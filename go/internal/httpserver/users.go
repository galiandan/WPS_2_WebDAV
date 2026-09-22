package httpserver

import (
	"errors"
	"net/http"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/accounts"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func (d *RESTDispatcher) SetUsers(store *accounts.Store, resolveRoot func(string) (model.RemoteEntry, error), binding ...func(string) (string, error)) {
	d.users = store
	d.userRootResolver = resolveRoot
	if len(binding) > 0 {
		d.userRootBindingResolver = binding[0]
	}
}
func (d *RESTDispatcher) serveUsers(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if d.users == nil || !ok || !principal.IsAdmin() {
		return model.NewStorageError(model.KindPermissionDenied, "access denied")
	}
	current, ok := d.users.LookupID(principal.ID)
	if !ok || current.PolicyVersion != principal.PolicyVersion {
		return model.NewStorageError(model.KindPermissionDenied, "access denied")
	}
	parts := strings.Split(route.Suffix, "/")
	if len(parts) > 2 {
		return model.NewStorageError(model.KindEntryNotFound, "unknown user route")
	}
	if r.Method == http.MethodGet && len(parts) == 1 {
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		return sendJSON(w, r, 200, map[string]any{"users": d.users.List()}, d.limits, nil)
	}
	if r.Method == http.MethodDelete && len(parts) == 2 {
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		if current, ok := d.users.LookupID(principal.ID); !ok || current.PolicyVersion != principal.PolicyVersion {
			return model.NewStorageError(model.KindPermissionDenied, "access denied")
		}
		if err := d.users.Delete(parts[1]); err != nil {
			return accountError(err)
		}
		writeResponse(w, r, 204, nil, contentTypeText, nil, false)
		return nil
	}
	if !(r.Method == http.MethodPost && len(parts) == 1 || r.Method == http.MethodPatch && len(parts) == 2) {
		return errBadRequest("unsupported users method")
	}
	payload, err := readJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	if len(payload) == 0 {
		return errBadRequest("account changes are required")
	}
	for key := range payload {
		if key != "username" && key != "password" && key != "root_path" && key != "permissions" && (key != "enabled" || r.Method != http.MethodPatch) {
			return errBadRequest("unknown user field")
		}
	}
	var update accounts.UpdateUser
	for key, target := range map[string]**string{"username": &update.Username, "password": &update.Password, "root_path": &update.RootPath} {
		if raw, exists := payload[key]; exists {
			value, ok := raw.(string)
			if !ok {
				return errBadRequest(key + " must be a string")
			}
			*target = &value
		}
	}
	if raw, exists := payload["enabled"]; exists {
		value, ok := raw.(bool)
		if !ok {
			return errBadRequest("enabled must be boolean")
		}
		update.Enabled = &value
	}
	if raw, exists := payload["permissions"]; exists {
		values, ok := raw.(map[string]any)
		if !ok || len(values) != 3 {
			return errBadRequest("permissions requires read, upload and delete booleans")
		}
		permissions := auth.Permissions{}
		for key, target := range map[string]*bool{"read": &permissions.Read, "upload": &permissions.Upload, "delete": &permissions.Delete} {
			value, ok := values[key].(bool)
			if !ok {
				return errBadRequest("permissions requires read, upload and delete booleans")
			}
			*target = value
		}
		update.Permissions = &permissions
	}
	if update.RootPath != nil {
		canonical, err := canonicalRemotePath(*update.RootPath)
		if err != nil {
			return err
		}
		update.RootPath = &canonical
		if d.userRootResolver == nil || d.userRootBindingResolver == nil {
			return model.NewStorageError(model.KindUnsupportedOperation, "account root selection is unavailable")
		}
		beforeBinding, err := d.userRootBindingResolver(canonical)
		if err != nil {
			return err
		}
		entry, err := d.userRootResolver(canonical)
		if err != nil {
			return err
		}
		if entry.Kind != model.KindFolder || entry.ID == "" {
			return model.NewStorageError(model.KindNotFolder, "account root must be a folder")
		}
		update.RootID = &entry.ID
		binding, err := d.userRootBindingResolver(canonical)
		if err != nil {
			return err
		}
		if binding != beforeBinding {
			return model.NewStorageError(model.KindAlreadyExists, "storage changed while selecting the account root")
		}
		update.RootBinding = &binding
	}
	if current, ok := d.users.LookupID(principal.ID); !ok || current.PolicyVersion != principal.PolicyVersion {
		return model.NewStorageError(model.KindPermissionDenied, "access denied")
	}
	var user accounts.User
	status := http.StatusOK
	if r.Method == http.MethodPost {
		if update.Username == nil || update.Password == nil || update.RootPath == nil || update.Permissions == nil {
			return errBadRequest("username, password, root_path and permissions are required")
		}
		user, err = d.users.Create(accounts.CreateUser{Username: *update.Username, Password: *update.Password, RootPath: *update.RootPath, RootID: *update.RootID, RootBinding: *update.RootBinding, Permissions: *update.Permissions})
		status = http.StatusCreated
	} else {
		user, err = d.users.Update(parts[1], update)
	}
	if err != nil {
		return accountError(err)
	}
	return sendJSON(w, r, status, map[string]any{"user": user}, d.limits, nil)
}
func accountError(err error) error {
	switch {
	case errors.Is(err, accounts.ErrInvalid):
		return errBadRequest("invalid account: username must use 1–64 letters, digits, dot, dash or underscore; password must use 8–256 bytes; read access and a folder root are required")
	case errors.Is(err, accounts.ErrConflict), errors.Is(err, accounts.ErrImmutable):
		return model.NewStorageError(model.KindAlreadyExists, err.Error())
	case errors.Is(err, accounts.ErrNotFound):
		return model.NewStorageError(model.KindEntryNotFound, err.Error())
	case errors.Is(err, accounts.ErrLimit), errors.Is(err, accounts.ErrBusy):
		return model.NewStorageError(model.KindServiceBusy, err.Error())
	default:
		return model.NewStorageError(model.KindIOFailure, "account state could not be saved")
	}
}
