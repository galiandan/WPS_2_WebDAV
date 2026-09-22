package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// ScopeBinding identifies the concrete WPS namespace behind a browser path.
// Root IDs such as "0" repeat across spaces; comparing the folder ID alone
// would silently follow a mount name remapped to another group. Credentials
// are intentionally absent so ordinary session refresh does not revoke users.
func (m *MultiSpace) ScopeBinding(path string) (string, error) {
	parts, err := SplitRemotePath(path)
	if err != nil {
		return "", err
	}
	if err := m.syncMounts(); err != nil {
		return "", err
	}
	m.mu.Lock()
	mounts := append([]Mount(nil), m.mounts...)
	spaces := make(map[string]*Storage, len(m.spaces))
	for name, space := range m.spaces {
		spaces[name] = space
	}
	single := m.single
	m.mu.Unlock()
	groupID, rootID := "", ""
	if len(mounts) > 0 {
		if len(parts) == 0 {
			return "", model.NewStorageError(model.KindPermissionDenied, "select one concrete WPS space or folder")
		}
		for _, mount := range mounts {
			if mount.Name == parts[0] {
				space := spaces[mount.Name]
				if space == nil {
					return "", model.NewStorageError(model.KindPermissionDenied, "storage namespace is unavailable")
				}
				root, err := space.Root()
				if err != nil {
					return "", err
				}
				groupID, rootID = mount.GroupID, root.ID
				break
			}
		}
	} else {
		if single == nil {
			return "", model.NewStorageError(model.KindPermissionDenied, "storage namespace is unavailable")
		}
		root, err := single.Root()
		if err != nil {
			return "", err
		}
		rootID = root.ID
		groupID = m.config.StaticGroupID
		if m.config.SingleSelection != nil {
			groupID, _, _, err = m.config.SingleSelection()
			if err != nil {
				return "", err
			}
		} else if m.config.MountsSource != nil {
			current, group, err := m.config.MountsSource()
			if err != nil {
				return "", err
			}
			if len(current) > 0 {
				return "", model.NewStorageError(model.KindPermissionDenied, "storage namespace changed")
			}
			groupID = group
		}
	}
	if groupID == "" || rootID == "" {
		return "", model.NewStorageError(model.KindPermissionDenied, "storage namespace is unavailable")
	}
	encoded, _ := json.Marshal(struct{ GroupID, RootID string }{groupID, rootID})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
