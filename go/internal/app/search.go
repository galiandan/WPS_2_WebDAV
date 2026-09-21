package app

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

// serveDAVWithSearch invalidates before and after a WebDAV file mutation, just
// like REST writes. The final invalidation discards scans started while a slow
// write was still in progress.
func (a *Application) serveDAVWithSearch(w http.ResponseWriter, r *http.Request, path string) error {
	switch r.Method {
	case "PUT", "MKCOL", "DELETE", "MOVE", "COPY":
		a.rest.InvalidateSearch()
		defer a.rest.InvalidateSearch()
	}
	return a.dav.ServeDAV(w, r, path)
}

// searchIdentity follows the same local credential and workspace sources used
// by WPS requests, so SSH-assisted login changes invalidate filename snapshots.
// Neither credential material nor the digest is exposed by the search API.
func (a *Application) searchIdentity() ([32]byte, error) {
	identity := struct {
		Cookie, CSRFToken, GroupID, RootID string
		Spaces                             []workspace.Mount
	}{GroupID: a.Config.GroupID, RootID: a.Config.RootID}
	if a.Source != nil {
		current, err := a.Source.Get()
		if err != nil {
			return [32]byte{}, err
		}
		if err := credentials.ValidateValues(current); err != nil {
			return [32]byte{}, err
		}
		identity.Cookie, identity.CSRFToken = current.Cookie, current.CSRFToken
	}
	if a.State != nil {
		var err error
		identity.GroupID, err = a.State.GroupID()
		if err != nil {
			return [32]byte{}, err
		}
		identity.RootID, err = a.State.RootID()
		if err != nil {
			return [32]byte{}, err
		}
		identity.Spaces, err = a.State.Spaces()
		if err != nil {
			return [32]byte{}, err
		}
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}
