package app

import (
	"crypto/sha256"
	"encoding/json"
)

// Bind persisted work to the credential/workspace snapshot and the configured
// WPS API realm. An endpoint or account-mode change also invalidates old tasks.
func (a *Application) taskIdentity() ([32]byte, error) {
	current, err := a.searchIdentity()
	if err != nil {
		return [32]byte{}, err
	}
	encoded, err := json.Marshal(struct {
		Snapshot                                    [32]byte
		Mode, BaseURL, AccountBaseURL, ObjectSuffix string
	}{current, a.Config.Mode, a.Config.BaseURL, a.Config.AccountBaseURL, a.Config.ObjectSuffix})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}
