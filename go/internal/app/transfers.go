package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/httpserver"
)

func (a *Application) initTransfers() error {
	if a.Accounts == nil {
		return nil
	}
	file := a.Config.TransfersFile
	if file == "" {
		file = filepath.Join(filepath.Dir(a.Config.WebSettingsDir), "transfers.json")
	}
	directory := a.Config.TransferDataDir
	if directory == "" {
		directory = filepath.Join(a.Budget.SpoolDir(), "transfers")
	}
	controller, err := httpserver.NewTransferController(httpserver.TransferConfig{
		File: file, Directory: directory, Budget: a.Budget, ResolveOwner: a.resolveShareOwner,
		DispatcherFor: a.transferDispatcher, Identity: a.transferIdentity,
	})
	if err != nil {
		return err
	}
	a.Transfers = controller
	return nil
}
func (a *Application) transferDispatcher(actor auth.Principal) (*httpserver.RESTDispatcher, error) {
	if _, err := a.resolveShareOwner(actor.ID, actor.PolicyVersion); err != nil {
		return nil, err
	}
	if actor.IsAdmin() {
		return a.rest, nil
	}
	member, err := a.member(actor)
	if err != nil {
		return nil, err
	}
	return member.rest, nil
}
func (a *Application) transferIdentity(actor auth.Principal, paths []string) (string, error) {
	if _, err := a.resolveShareOwner(actor.ID, actor.PolicyVersion); err != nil {
		return "", err
	}
	bindings := make([]string, 0, len(paths))
	for _, path := range paths {
		binding, err := a.shareBinding(actor, path)
		if err != nil {
			return "", err
		}
		bindings = append(bindings, binding)
	}
	encoded, err := json.Marshal(struct {
		Owner           auth.Principal
		Paths, Bindings []string
	}{actor, paths, bindings})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
