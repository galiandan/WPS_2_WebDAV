package storage

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// multiSpaceRootID is the fixed virtual root id of the multi-space view,
// mirroring Python's MultiSpaceStorage.root_id.
const multiSpaceRootID = "multi-space-root"

// Mount describes one WPS space exposed below the virtual root. It mirrors
// the duck-typed mounts MultiSpaceStorage accepts; the app layer converts
// workspace mounts into these.
type Mount struct {
	Name    string
	GroupID string
	RootID  string
}

// SpaceClients is the WPS surface of one mounted space.
type SpaceClients struct {
	Lister     Lister
	Writer     Writer
	Downloader Downloader
}

// SpaceFactory builds the client surfaces for one space group. The empty
// group id requests the base client used by the no-mounts single-space
// fallback, mirroring Python's reuse of the full client there. All spaces
// share the process budget injected into NewMultiSpace and the same
// credential source behind the factory, so limits and refresh coordination
// never scale with the mount count.
type SpaceFactory func(groupID string) (SpaceClients, error)

// MultiSpaceConfig mirrors MultiSpaceStorage.__init__'s parameters.
type MultiSpaceConfig struct {
	RootName     string
	SingleRootID string

	// MountsSource reports the hot workspace mounts and the resolved group
	// id. nil disables hot sync: StaticMounts and StaticGroupID are used
	// as-is, mirroring Python's missing client.config.workspace.
	MountsSource  func() (mounts []Mount, groupID string, err error)
	StaticMounts  []Mount
	StaticGroupID string

	// SingleSelection feeds the no-mounts fallback storage the same way
	// StorageConfig.WorkspaceSelection does; it also supplies the current
	// workspace root used when building that storage. nil means the
	// fallback uses SingleRootID and never syncs (the no-workspace case).
	SingleSelection func() (groupID string, rootID string, autoRoot bool, err error)

	SpaceFactory SpaceFactory

	// Space is the per-space Storage template. RootID, RootName, Writer,
	// Downloader, and WorkspaceSelection are overwritten per space.
	Space StorageConfig
}

// NewMultiSpace validates the configuration and builds the initial routing.
func NewMultiSpace(transferBudget *budget.Budget, config MultiSpaceConfig) (*MultiSpace, error) {
	if transferBudget == nil {
		return nil, errors.New("a transfer budget is required")
	}
	if config.RootName == "" {
		config.RootName = "WPS Enterprise Drive"
	}
	if config.SingleRootID == "" {
		config.SingleRootID = "0"
	}
	if config.SpaceFactory == nil {
		return nil, errors.New("a space factory is required")
	}
	mounts := config.StaticMounts
	groupID := config.StaticGroupID
	if config.MountsSource != nil {
		var err error
		mounts, groupID, err = config.MountsSource()
		if err != nil {
			return nil, err
		}
	}
	multi := &MultiSpace{
		budget:       transferBudget,
		rootName:     config.RootName,
		singleRootID: config.SingleRootID,
		config:       config,
		spaces:       map[string]*Storage{},
	}
	if err := multi.rebuild(mounts, groupID); err != nil {
		return nil, err
	}
	return multi, nil
}

// MultiSpace exposes selected WPS spaces as folders below one virtual root.
// It is safe for concurrent use: hot workspace updates rebuild the routing
// atomically, and every space shares the process-wide transfer budget.
type MultiSpace struct {
	budget *budget.Budget

	mu       sync.Mutex
	rootName string
	mounts   []Mount
	spaces   map[string]*Storage
	single   *Storage

	singleRootID string
	config       MultiSpaceConfig
}

// rebuild builds the full routing for the given mounts and swaps it in
// atomically. On failure the previous routing keeps serving; the next call
// retries the rebuild because the mounts still differ.
func (m *MultiSpace) rebuild(mounts []Mount, groupID string) error {
	for i, mount := range mounts {
		for _, other := range mounts[i+1:] {
			if other.Name == mount.Name {
				return errors.New("WPS space names must be unique")
			}
		}
	}
	spaces := make(map[string]*Storage, len(mounts))
	var single *Storage
	if len(mounts) == 0 {
		hasGroup := groupID != ""
		rootID := m.singleRootID
		selection := m.config.SingleSelection
		if selection != nil {
			selectedGroup, selectedRoot, _, err := selection()
			if err != nil {
				return err
			}
			if selectedGroup != "" {
				hasGroup = true
			}
			rootID = selectedRoot
		}
		if hasGroup {
			clients, err := m.config.SpaceFactory("")
			if err != nil {
				return err
			}
			spaceConfig := m.config.Space
			spaceConfig.RootID = rootID
			spaceConfig.RootName = m.rootName
			spaceConfig.Writer = clients.Writer
			spaceConfig.Downloader = clients.Downloader
			spaceConfig.WorkspaceSelection = selection
			single, err = NewStorage(clients.Lister, m.budget, spaceConfig)
			if err != nil {
				return err
			}
		}
	} else {
		for _, mount := range mounts {
			clients, err := m.config.SpaceFactory(mount.GroupID)
			if err != nil {
				return err
			}
			spaceConfig := m.config.Space
			spaceConfig.RootID = mount.RootID
			spaceConfig.RootName = mount.Name
			spaceConfig.Writer = clients.Writer
			spaceConfig.Downloader = clients.Downloader
			spaceConfig.WorkspaceSelection = nil
			space, err := NewStorage(clients.Lister, m.budget, spaceConfig)
			if err != nil {
				return err
			}
			spaces[mount.Name] = space
		}
	}

	m.mu.Lock()
	m.mounts = mounts
	m.spaces = spaces
	m.single = single
	m.mu.Unlock()
	return nil
}

// syncMounts mirrors _sync_mounts: rebuild when the mounts changed or when
// a pending group appeared for an empty mount list. A failed rebuild keeps
// the previous routing in place (the mounts still differ, so the next call
// retries) and reports the error, instead of Python's half-updated state.
func (m *MultiSpace) syncMounts() error {
	if m.config.MountsSource == nil {
		return nil
	}
	mounts, groupID, err := m.config.MountsSource()
	if err != nil {
		return err
	}
	m.mu.Lock()
	mountsChanged := !mountsEqual(mounts, m.mounts)
	pendingGroup := len(mounts) == 0 && m.single == nil && groupID != ""
	m.mu.Unlock()
	if !mountsChanged && !pendingGroup {
		return nil
	}
	return m.rebuild(mounts, groupID)
}

func mountsEqual(a []Mount, b []Mount) bool {
	return slices.EqualFunc(a, b, func(x Mount, y Mount) bool {
		return x == y
	})
}

// Root returns the fixed virtual root entry.
func (m *MultiSpace) Root() (model.RemoteEntry, error) {
	if err := m.syncMounts(); err != nil {
		return model.RemoteEntry{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return model.RemoteEntry{
		ID:   multiSpaceRootID,
		Name: m.rootName,
		Kind: model.KindFolder,
		Size: model.Ptr(int64(0)),
	}, nil
}

// StatusRootID mirrors the status_root_id property: the single storage's
// root, else the first mount's root, else "0".
func (m *MultiSpace) StatusRootID() (string, error) {
	if err := m.syncMounts(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.single != nil {
		return m.single.RootID(), nil
	}
	if len(m.mounts) > 0 {
		return m.mounts[0].RootID, nil
	}
	return "0", nil
}

// SetRootID updates the single-root view; named spaces keep their own roots.
func (m *MultiSpace) SetRootID(rootID string) error {
	if err := m.syncMounts(); err != nil {
		return err
	}
	m.mu.Lock()
	single := m.single
	m.mu.Unlock()
	if single != nil {
		return single.SetRootID(rootID)
	}
	return nil
}

// SetRootName updates the display name of the virtual root and of the
// single-space fallback.
func (m *MultiSpace) SetRootName(rootName string) error {
	if rootName == "" {
		return errors.New("root_name is required")
	}
	m.mu.Lock()
	m.rootName = rootName
	single := m.single
	m.mu.Unlock()
	if single != nil {
		return single.SetRootName(rootName)
	}
	return nil
}

func (m *MultiSpace) singleOrFail() (*Storage, error) {
	if err := m.syncMounts(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	single := m.single
	m.mu.Unlock()
	if single == nil {
		return nil, model.NewStorageError(model.KindEntryNotFound, "WPS workspace is not configured")
	}
	return single, nil
}

// route mirrors _route: the first path segment selects the space, the rest
// is the business path inside it.
func (m *MultiSpace) route(path string) (*Storage, string, error) {
	parts, err := SplitRemotePath(path)
	if err != nil {
		return nil, "", err
	}
	m.mu.Lock()
	var space *Storage
	if len(parts) > 0 {
		space = m.spaces[parts[0]]
	}
	m.mu.Unlock()
	if space == nil {
		name := ""
		if len(parts) > 0 {
			name = parts[0]
		}
		return nil, "", model.NewStorageError(model.KindEntryNotFound, "WPS space not found: "+name)
	}
	childPath, err := JoinRemotePath(parts[1:], false)
	if err != nil {
		return nil, "", err
	}
	return space, childPath, nil
}

func (m *MultiSpace) mountByName(name string) (Mount, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mount := range m.mounts {
		if mount.Name == name {
			return mount, true
		}
	}
	return Mount{}, false
}

func spaceEntry(mount Mount) model.RemoteEntry {
	return model.RemoteEntry{
		ID:       "space:" + mount.GroupID,
		Name:     mount.Name,
		Kind:     model.KindFolder,
		ParentID: model.Ptr(multiSpaceRootID),
		Size:     model.Ptr(int64(0)),
	}
}

// Metadata resolves path inside the multi-space view. The root and the
// space roots are virtual entries that never touch WPS.
func (m *MultiSpace) Metadata(path string) (model.RemoteEntry, error) {
	if err := m.syncMounts(); err != nil {
		return model.RemoteEntry{}, err
	}
	parts, err := SplitRemotePath(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if len(parts) == 0 {
		return m.Root()
	}
	m.mu.Lock()
	hasMounts := len(m.mounts) > 0
	single := m.single
	m.mu.Unlock()
	if !hasMounts {
		return singleOrFailEntry(single, path)
	}
	space, childPath, err := m.route(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	childParts, err := SplitRemotePath(childPath)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if len(childParts) == 0 {
		mount, ok := m.mountByName(parts[0])
		if !ok {
			return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "WPS space not found: "+parts[0])
		}
		return spaceEntry(mount), nil
	}
	return space.Metadata(childPath)
}

func singleOrFailEntry(single *Storage, path string) (model.RemoteEntry, error) {
	if single == nil {
		return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "WPS workspace is not configured")
	}
	return single.Metadata(path)
}

// ListPath lists the children of path. The root listing returns only the
// configured mounts (or the single space's root listing) and never talks to
// WPS for the mount entries themselves.
func (m *MultiSpace) ListPath(path string) ([]model.RemoteEntry, error) {
	if err := m.syncMounts(); err != nil {
		return nil, err
	}
	parts, err := SplitRemotePath(path)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		m.mu.Lock()
		single := m.single
		mounts := m.mounts
		m.mu.Unlock()
		if single != nil {
			return single.ListPath("/")
		}
		entries := make([]model.RemoteEntry, 0, len(mounts))
		for _, mount := range mounts {
			entries = append(entries, spaceEntry(mount))
		}
		return entries, nil
	}
	m.mu.Lock()
	hasMounts := len(m.mounts) > 0
	single := m.single
	m.mu.Unlock()
	if !hasMounts {
		if single == nil {
			return nil, model.NewStorageError(model.KindEntryNotFound, "WPS workspace is not configured")
		}
		return single.ListPath(path)
	}
	space, childPath, err := m.route(path)
	if err != nil {
		return nil, err
	}
	return space.ListPath(childPath)
}

// UploadPath uploads into the routed space; the root itself is rejected by
// routing or by the single storage's parent rules.
func (m *MultiSpace) UploadPath(ctx context.Context, path string, source io.Reader, options UploadOptions) (model.RemoteEntry, error) {
	if err := m.syncMounts(); err != nil {
		return model.RemoteEntry{}, err
	}
	m.mu.Lock()
	hasMounts := len(m.mounts) > 0
	m.mu.Unlock()
	if !hasMounts {
		single, err := m.singleOrFail()
		if err != nil {
			return model.RemoteEntry{}, err
		}
		return single.UploadPath(ctx, path, source, options)
	}
	space, childPath, err := m.route(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	return space.UploadPath(ctx, childPath, source, options)
}

// CreateFolderPath creates a folder inside the routed space.
func (m *MultiSpace) CreateFolderPath(path string) (model.RemoteEntry, error) {
	if err := m.syncMounts(); err != nil {
		return model.RemoteEntry{}, err
	}
	m.mu.Lock()
	hasMounts := len(m.mounts) > 0
	m.mu.Unlock()
	if !hasMounts {
		single, err := m.singleOrFail()
		if err != nil {
			return model.RemoteEntry{}, err
		}
		return single.CreateFolderPath(path)
	}
	space, childPath, err := m.route(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	return space.CreateFolderPath(childPath)
}

// OpenPath opens a download inside the routed space.
func (m *MultiSpace) OpenPath(ctx context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	if err := m.syncMounts(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	hasMounts := len(m.mounts) > 0
	m.mu.Unlock()
	if !hasMounts {
		single, err := m.singleOrFail()
		if err != nil {
			return nil, err
		}
		return single.OpenPath(ctx, path, offset, length)
	}
	space, childPath, err := m.route(path)
	if err != nil {
		return nil, err
	}
	return space.OpenPath(ctx, childPath, offset, length)
}

// DeletePath deletes inside the routed space.
func (m *MultiSpace) DeletePath(path string) error {
	if err := m.syncMounts(); err != nil {
		return err
	}
	m.mu.Lock()
	hasMounts := len(m.mounts) > 0
	m.mu.Unlock()
	if !hasMounts {
		single, err := m.singleOrFail()
		if err != nil {
			return err
		}
		return single.DeletePath(path)
	}
	space, childPath, err := m.route(path)
	if err != nil {
		return err
	}
	return space.DeletePath(childPath)
}

// RenamePath renames inside the routed space.
func (m *MultiSpace) RenamePath(path string, name string) (model.RemoteEntry, error) {
	if err := m.syncMounts(); err != nil {
		return model.RemoteEntry{}, err
	}
	m.mu.Lock()
	hasMounts := len(m.mounts) > 0
	m.mu.Unlock()
	if !hasMounts {
		single, err := m.singleOrFail()
		if err != nil {
			return model.RemoteEntry{}, err
		}
		return single.RenamePath(path, name)
	}
	space, childPath, err := m.route(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	return space.RenamePath(childPath, name)
}

// MovePath moves inside one space; a cross-space move is unsupported.
func (m *MultiSpace) MovePath(path string, destination string) (model.RemoteEntry, error) {
	if err := m.syncMounts(); err != nil {
		return model.RemoteEntry{}, err
	}
	m.mu.Lock()
	hasMounts := len(m.mounts) > 0
	m.mu.Unlock()
	if !hasMounts {
		single, err := m.singleOrFail()
		if err != nil {
			return model.RemoteEntry{}, err
		}
		return single.MovePath(path, destination)
	}
	sourceSpace, sourceChild, err := m.route(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	destinationSpace, destinationChild, err := m.route(destination)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if sourceSpace != destinationSpace {
		return model.RemoteEntry{}, model.NewStorageError(model.KindUnsupportedOperation, "cross-space move is not supported")
	}
	return sourceSpace.MovePath(sourceChild, destinationChild)
}

// MoveToParentPath moves inside one space; a cross-space move is
// unsupported.
func (m *MultiSpace) MoveToParentPath(path string, parentPath string) (model.RemoteEntry, error) {
	if err := m.syncMounts(); err != nil {
		return model.RemoteEntry{}, err
	}
	m.mu.Lock()
	hasMounts := len(m.mounts) > 0
	m.mu.Unlock()
	if !hasMounts {
		single, err := m.singleOrFail()
		if err != nil {
			return model.RemoteEntry{}, err
		}
		return single.MoveToParentPath(path, parentPath)
	}
	sourceSpace, sourceChild, err := m.route(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	destinationSpace, destinationChild, err := m.route(parentPath)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if sourceSpace != destinationSpace {
		return model.RemoteEntry{}, model.NewStorageError(model.KindUnsupportedOperation, "cross-space move is not supported")
	}
	return sourceSpace.MoveToParentPath(sourceChild, destinationChild)
}
