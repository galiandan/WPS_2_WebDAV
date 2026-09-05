package storage

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/cache"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/wps"
)

// Lister is the read surface of the WPS client used for path resolution and
// cached listings. *wps.Client satisfies it.
type Lister interface {
	IterEntries(parentID string, options wps.IterOptions) ([]model.RemoteEntry, error)
}

// UploadRequest mirrors client.upload's keyword surface.
type UploadRequest struct {
	ParentID string
	Name     string
	Source   io.Reader
	Size     *int64
	// ContentType and CSRFToken mirror the Python None-or-string keywords:
	// empty means absent.
	ContentType string
	CSRFToken   string
	Overwrite   bool
}

// Writer is the write surface of the WPS client consumed by the mutation
// methods. B503 defines the interface and runs every conflict check around
// it; the wps package implements it in the later write stages, so a Storage
// built without a Writer refuses writes with a fixed unsupported error.
type Writer interface {
	Upload(request UploadRequest) (model.RemoteEntry, error)
	CreateFolder(parentID string, name string) (model.RemoteEntry, error)
	Delete(entryID string) error
	Rename(entryID string, name string) (model.RemoteEntry, error)
	Move(entryID string, sourceParentID string, destinationParentID string) error
}

// Downloader opens upstream download streams. It is optional until the
// download stage wires the real client.
type Downloader interface {
	OpenDownload(entryID string, offset int64, length *int64, cid *string) (DownloadStream, error)
}

// DownloadStream is the upstream download surface the HTTP layer consumes.
// Pointer accessors mirror Python's None-or-value properties.
type DownloadStream interface {
	io.ReadCloser
	HTTPStatus() int
	ContentType() *string
	ContentLength() *int64
	ContentRange() *string
}

// StorageConfig mirrors WpsStorage.__init__'s keyword parameters. The
// per-process transfer limits live in the injected budget (D-03), not here.
type StorageConfig struct {
	RootID   string
	RootName string

	ListCount      int
	MaxListEntries int

	CacheTTLSeconds  float64
	MaxCachedFolders int

	TransferWaitTimeout float64

	MaxCopyEntries int
	MaxCopyDepth   int

	// Writer and Downloader are the optional WPS write and download
	// surfaces; nil means the corresponding operations refuse to run.
	Writer     Writer
	Downloader Downloader

	// WorkspaceSelection reports the hot-reloaded workspace mapping:
	// selected group ID, selected root ID, and whether the root follows
	// login selections (configured root "auto"). nil disables the sync,
	// mirroring Python's missing client.config.workspace.
	WorkspaceSelection func() (groupID string, rootID string, autoRoot bool, err error)
}

// DefaultStorageConfig mirrors the Python keyword defaults for one root.
func DefaultStorageConfig(rootID string) StorageConfig {
	return StorageConfig{
		RootID:              rootID,
		RootName:            "WPS Enterprise Drive",
		ListCount:           20,
		MaxListEntries:      10000,
		CacheTTLSeconds:     2.0,
		MaxCachedFolders:    1024,
		TransferWaitTimeout: 30.0,
		MaxCopyEntries:      10000,
		MaxCopyDepth:        64,
	}
}

// NewStorage validates the configuration and builds the single-space
// storage facade. transferBudget must be the process-wide budget shared by
// every mounted space.
func NewStorage(lister Lister, transferBudget *budget.Budget, config StorageConfig) (*Storage, error) {
	if lister == nil {
		return nil, errors.New("a wps lister is required")
	}
	if transferBudget == nil {
		return nil, errors.New("a transfer budget is required")
	}
	if config.RootID == "" {
		return nil, errors.New("root_id is required")
	}
	if config.ListCount == 0 {
		config.ListCount = 20
	}
	if config.MaxListEntries == 0 {
		config.MaxListEntries = 10000
	}
	if config.CacheTTLSeconds == 0 {
		config.CacheTTLSeconds = 2.0
	}
	if config.MaxCachedFolders == 0 {
		config.MaxCachedFolders = 1024
	}
	if config.TransferWaitTimeout == 0 {
		config.TransferWaitTimeout = 30.0
	}
	if config.MaxCopyEntries == 0 {
		config.MaxCopyEntries = 10000
	}
	if config.MaxCopyDepth == 0 {
		config.MaxCopyDepth = 64
	}
	if config.ListCount <= 0 {
		return nil, errors.New("list_count must be positive")
	}
	if config.MaxListEntries <= 0 {
		return nil, errors.New("max_list_entries must be positive")
	}
	if config.ListCount > config.MaxListEntries {
		return nil, errors.New("list_count must not exceed max_list_entries")
	}
	if config.CacheTTLSeconds < 0 {
		return nil, errors.New("cache_ttl must not be negative")
	}
	if config.MaxCachedFolders <= 0 {
		return nil, errors.New("max_cached_folders must be positive")
	}
	if config.TransferWaitTimeout <= 0 {
		return nil, errors.New("transfer_wait_timeout must be positive")
	}
	if config.MaxCopyEntries <= 0 {
		return nil, errors.New("max_copy_entries must be positive")
	}
	if config.MaxCopyDepth <= 0 {
		return nil, errors.New("max_copy_depth must be positive")
	}
	folderCache, err := cache.New(cache.Options{
		TTL:        time.Duration(config.CacheTTLSeconds * float64(time.Second)),
		MaxFolders: config.MaxCachedFolders,
	})
	if err != nil {
		return nil, err
	}
	return &Storage{
		lister:             lister,
		writer:             config.Writer,
		downloader:         config.Downloader,
		budget:             transferBudget,
		folders:            folderCache,
		listCount:          config.ListCount,
		maxListEntries:     config.MaxListEntries,
		transferWait:       time.Duration(config.TransferWaitTimeout * float64(time.Second)),
		maxCopyEntries:     config.MaxCopyEntries,
		maxCopyDepth:       config.MaxCopyDepth,
		rootID:             config.RootID,
		rootName:           config.RootName,
		workspaceSelection: config.WorkspaceSelection,
	}, nil
}

// Storage is the path-aware facade over one confirmed WPS root. It is safe
// for concurrent use.
type Storage struct {
	lister         Lister
	writer         Writer
	downloader     Downloader
	budget         *budget.Budget
	folders        *cache.Cache
	listCount      int
	maxListEntries int
	transferWait   time.Duration
	maxCopyEntries int
	maxCopyDepth   int

	mu       sync.Mutex
	rootID   string
	rootName string
	groupID  string

	workspaceSelection func() (groupID string, rootID string, autoRoot bool, err error)
}

// errWritesNotWired refuses writes when no Writer is wired yet; the write
// stages replace this path with the real client implementation.
var errWritesNotWired = model.NewStorageError(model.KindUnsupportedOperation, "write operations are not wired in this stage")

// errDownloadsNotWired refuses downloads when no Downloader is wired yet.
var errDownloadsNotWired = model.NewStorageError(model.KindUnsupportedOperation, "download operations are not wired in this stage")

// syncWorkspaceRoot follows login-driven workspace remaps exactly like
// storage.py's _sync_workspace_root: only auto-configured roots follow the
// selection, and a group or root change clears the metadata cache.
func (s *Storage) syncWorkspaceRoot() error {
	if s.workspaceSelection == nil {
		return nil
	}
	groupID, rootID, autoRoot, err := s.workspaceSelection()
	if err != nil {
		return err
	}
	if !autoRoot {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if groupID != s.groupID {
		s.folders.Invalidate()
		s.groupID = groupID
	}
	if rootID != s.rootID {
		s.rootID = rootID
		s.folders.Invalidate()
	}
	return nil
}

// Root returns the virtual root entry after following any workspace remap.
func (s *Storage) Root() (model.RemoteEntry, error) {
	if err := s.syncWorkspaceRoot(); err != nil {
		return model.RemoteEntry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return model.RemoteEntry{
		ID:   s.rootID,
		Name: s.rootName,
		Kind: model.KindFolder,
		Size: model.Ptr(int64(0)),
	}, nil
}

// children mirrors _children: the cached, complete listing of one folder.
func (s *Storage) children(parentID string) ([]model.RemoteEntry, error) {
	if err := s.syncWorkspaceRoot(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	groupID := s.groupID
	s.mu.Unlock()
	key := cache.Key{GroupID: groupID, Generation: s.folders.Generation(), ParentID: parentID}
	return s.folders.GetOrLoad(key, func() ([]model.RemoteEntry, error) {
		return s.lister.IterEntries(parentID, wps.IterOptions{
			Count:               s.listCount,
			MaxEntries:          model.Ptr(s.maxListEntries),
			LinkGroup:           model.Ptr(true),
			Include:             model.Ptr("acl,pic_thumbnail"),
			WithLink:            model.Ptr(true),
			ReviewPicThumbnail:  model.Ptr(true),
			WithSharefolderType: model.Ptr(true),
		})
	})
}

// child mirrors _child's matching rules: exact name, unknown kinds never
// match, and a parent mismatch is reported as not found.
func child(parentID string, name string, entries []model.RemoteEntry) (model.RemoteEntry, error) {
	matches := make([]model.RemoteEntry, 0, 2)
	for _, entry := range entries {
		if entry.Name == name && entry.Kind != model.KindUnknown {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "entry not found: "+name)
	}
	if len(matches) > 1 {
		return model.RemoteEntry{}, model.NewStorageError(model.KindAmbiguousPath, "multiple entries have the name: "+name)
	}
	entry := matches[0]
	if entry.ParentID != nil && *entry.ParentID != parentID {
		return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "entry not found: "+name)
	}
	return entry, nil
}

func (s *Storage) resolveParts(parts []string) (model.RemoteEntry, error) {
	current, err := s.Root()
	if err != nil {
		return model.RemoteEntry{}, err
	}
	for _, name := range parts {
		if current.Kind != model.KindFolder {
			return model.RemoteEntry{}, model.NewStorageError(model.KindNotFolder, "not a folder: "+current.Name)
		}
		children, err := s.children(current.ID)
		if err != nil {
			return model.RemoteEntry{}, err
		}
		current, err = child(current.ID, name, children)
		if err != nil {
			return model.RemoteEntry{}, err
		}
	}
	return current, nil
}

// Resolve resolves one business path to its entry.
func (s *Storage) Resolve(path string) (model.RemoteEntry, error) {
	parts, err := SplitRemotePath(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	return s.resolveParts(parts)
}

// Metadata mirrors metadata: resolution is the metadata.
func (s *Storage) Metadata(path string) (model.RemoteEntry, error) {
	return s.Resolve(path)
}

// ListPath lists the children of the folder at path.
func (s *Storage) ListPath(path string) ([]model.RemoteEntry, error) {
	entry, err := s.Resolve(path)
	if err != nil {
		return nil, err
	}
	if entry.Kind != model.KindFolder {
		return nil, model.NewStorageError(model.KindNotFolder, "not a folder: "+path)
	}
	return s.children(entry.ID)
}

// ListByID mirrors list: children of an explicit folder ID, or of the
// virtual root when parentID is nil.
func (s *Storage) ListByID(parentID *string) ([]model.RemoteEntry, error) {
	if err := s.syncWorkspaceRoot(); err != nil {
		return nil, err
	}
	if parentID == nil {
		s.mu.Lock()
		rootID := s.rootID
		s.mu.Unlock()
		return s.children(rootID)
	}
	return s.children(*parentID)
}

func (s *Storage) parentAndName(path string) (model.RemoteEntry, string, []string, error) {
	parts, err := SplitRemotePath(path)
	if err != nil {
		return model.RemoteEntry{}, "", nil, err
	}
	if len(parts) == 0 {
		return model.RemoteEntry{}, "", nil, model.NewStorageError(model.KindInvalidPath, "the root cannot be used as a file name")
	}
	parentParts := parts[:len(parts)-1]
	parent, err := s.resolveParts(parentParts)
	if err != nil {
		return model.RemoteEntry{}, "", nil, err
	}
	if parent.Kind != model.KindFolder {
		canonical, joinErr := JoinRemotePath(parentParts, false)
		if joinErr != nil {
			return model.RemoteEntry{}, "", nil, joinErr
		}
		return model.RemoteEntry{}, "", nil, model.NewStorageError(model.KindNotFolder, "not a folder: "+canonical)
	}
	return parent, parts[len(parts)-1], parts, nil
}

func (s *Storage) invalidate() {
	s.folders.Invalidate()
}

// RootID returns the current virtual root id (hot-updated for auto roots).
func (s *Storage) RootID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rootID
}

// SetRootID mirrors set_root_id: switch the mapped WPS root after a
// login-selected workspace update and drop every cached listing.
func (s *Storage) SetRootID(rootID string) error {
	if rootID == "" {
		return errors.New("root_id is required")
	}
	s.mu.Lock()
	s.rootID = rootID
	s.mu.Unlock()
	s.invalidate()
	return nil
}

// SetRootName mirrors set_root_name: update the adapter-side display name
// for the virtual root. The cache is not invalidated.
func (s *Storage) SetRootName(rootName string) error {
	if rootName == "" {
		return errors.New("root_name is required")
	}
	s.mu.Lock()
	s.rootName = rootName
	s.mu.Unlock()
	return nil
}

// UploadPath uploads a new file at path, rejecting an existing entry unless
// overwrite targets exactly one existing file. The upload slot is held for
// the duration of the writer call.
func (s *Storage) UploadPath(ctx context.Context, path string, source io.Reader, options UploadOptions) (model.RemoteEntry, error) {
	parent, name, _, err := s.parentAndName(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	children, err := s.children(parent.ID)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	var existing []model.RemoteEntry
	for _, entry := range children {
		if entry.Name == name {
			existing = append(existing, entry)
		}
	}
	if len(existing) > 0 {
		if !options.Overwrite || len(existing) > 1 || existing[0].Kind != model.KindFile {
			return model.RemoteEntry{}, model.NewStorageError(model.KindAlreadyExists, "overwrite is not enabled for: "+path)
		}
	}
	release, err := s.budget.AcquireUpload(ctx)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	defer release()
	if s.writer == nil {
		return model.RemoteEntry{}, errWritesNotWired
	}
	result, err := s.writer.Upload(UploadRequest{
		ParentID:    parent.ID,
		Name:        name,
		Source:      source,
		Size:        options.Size,
		ContentType: options.ContentType,
		CSRFToken:   options.CSRFToken,
		Overwrite:   options.Overwrite,
	})
	if err != nil {
		return model.RemoteEntry{}, err
	}
	s.invalidate()
	return result, nil
}

// UploadOptions carries upload_path's optional keyword surface.
type UploadOptions struct {
	Size        *int64
	ContentType string
	CSRFToken   string
	Overwrite   bool
}

// CreateFolder creates a folder under an explicit parent ID (nil means the
// virtual root) after a collision check.
func (s *Storage) CreateFolder(parentID *string, name string) (model.RemoteEntry, error) {
	if err := s.syncWorkspaceRoot(); err != nil {
		return model.RemoteEntry{}, err
	}
	var parent string
	if parentID == nil {
		s.mu.Lock()
		parent = s.rootID
		s.mu.Unlock()
	} else {
		parent = *parentID
	}
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return model.RemoteEntry{}, model.NewStorageError(model.KindInvalidPath, "folder name must be one remote path component")
	}
	children, err := s.children(parent)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	for _, entry := range children {
		if entry.Name == name {
			return model.RemoteEntry{}, model.NewStorageError(model.KindAlreadyExists, "entry already exists: "+name)
		}
	}
	if s.writer == nil {
		return model.RemoteEntry{}, errWritesNotWired
	}
	result, err := s.writer.CreateFolder(parent, name)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	s.invalidate()
	return result, nil
}

// CreateFolderPath creates the folder at path after a collision check.
func (s *Storage) CreateFolderPath(path string) (model.RemoteEntry, error) {
	parent, name, _, err := s.parentAndName(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	children, err := s.children(parent.ID)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	for _, entry := range children {
		if entry.Name == name {
			return model.RemoteEntry{}, model.NewStorageError(model.KindAlreadyExists, "entry already exists: "+path)
		}
	}
	if s.writer == nil {
		return model.RemoteEntry{}, errWritesNotWired
	}
	result, err := s.writer.CreateFolder(parent.ID, name)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	s.invalidate()
	return result, nil
}

// OpenDownload mirrors open_download: opening by entry ID without binding a
// download slot.
func (s *Storage) OpenDownload(entryID string, offset int64) (DownloadStream, error) {
	if s.downloader == nil {
		return nil, errDownloadsNotWired
	}
	return s.downloader.OpenDownload(entryID, offset, nil, nil)
}

// OpenPath resolves path to a file and opens its download stream with a
// held download slot; the slot is released when the stream is closed or
// when opening fails.
func (s *Storage) OpenPath(ctx context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	entry, err := s.Resolve(path)
	if err != nil {
		return nil, err
	}
	if entry.Kind != model.KindFile {
		return nil, model.NewStorageError(model.KindNotFolder, "not a downloadable file: "+path)
	}
	release, err := s.budget.AcquireDownload(ctx)
	if err != nil {
		return nil, err
	}
	if s.downloader == nil {
		release()
		return nil, errDownloadsNotWired
	}
	stream, err := s.downloader.OpenDownload(entry.ID, offset, length, entry.LinkID)
	if err != nil {
		release()
		return nil, err
	}
	return &managedDownloadStream{stream: stream, release: release}, nil
}

// managedDownloadStream releases its download slot exactly once, on the
// first close.
type managedDownloadStream struct {
	stream  DownloadStream
	release func()
	once    sync.Once
}

func (m *managedDownloadStream) Read(p []byte) (int, error) {
	return m.stream.Read(p)
}

func (m *managedDownloadStream) HTTPStatus() int {
	return m.stream.HTTPStatus()
}

func (m *managedDownloadStream) ContentType() *string {
	return m.stream.ContentType()
}

func (m *managedDownloadStream) ContentLength() *int64 {
	return m.stream.ContentLength()
}

func (m *managedDownloadStream) ContentRange() *string {
	return m.stream.ContentRange()
}

func (m *managedDownloadStream) Close() error {
	var err error
	m.once.Do(func() {
		err = m.stream.Close()
		m.release()
	})
	return err
}

// Delete removes an entry by ID and invalidates the cache.
func (s *Storage) Delete(entryID string) error {
	if s.writer == nil {
		return errWritesNotWired
	}
	if err := s.writer.Delete(entryID); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

// DeletePath removes the entry at path; the root cannot be deleted.
func (s *Storage) DeletePath(path string) error {
	parts, err := SplitRemotePath(path)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return model.NewStorageError(model.KindInvalidPath, "the root cannot be deleted")
	}
	entry, err := s.Resolve(path)
	if err != nil {
		return err
	}
	if s.writer == nil {
		return errWritesNotWired
	}
	if err := s.writer.Delete(entry.ID); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

// validateEntryName mirrors _validate_entry_name.
func validateEntryName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.Contains(name, "/") || containsForbiddenChar(name) ||
		len(name) > MaxRemoteNameBytes {
		return model.NewStorageError(model.KindInvalidPath, "name must be one remote path component")
	}
	return nil
}

// Rename renames an entry by ID.
func (s *Storage) Rename(entryID string, name string) (model.RemoteEntry, error) {
	if err := validateEntryName(name); err != nil {
		return model.RemoteEntry{}, err
	}
	if s.writer == nil {
		return model.RemoteEntry{}, errWritesNotWired
	}
	result, err := s.writer.Rename(entryID, name)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	s.invalidate()
	return result, nil
}

// RenamePath renames the entry at path. Renaming to the same name is a
// no-op that never calls the writer.
func (s *Storage) RenamePath(path string, name string) (model.RemoteEntry, error) {
	parts, err := SplitRemotePath(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if len(parts) == 0 {
		return model.RemoteEntry{}, model.NewStorageError(model.KindInvalidPath, "the root cannot be renamed")
	}
	if err := validateEntryName(name); err != nil {
		return model.RemoteEntry{}, err
	}
	entry, err := s.Resolve(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	parent, err := s.resolveParts(parts[:len(parts)-1])
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if name == entry.Name {
		return entry, nil
	}
	children, err := s.children(parent.ID)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	for _, sibling := range children {
		if sibling.Name == name && sibling.ID != entry.ID {
			return model.RemoteEntry{}, model.NewStorageError(model.KindAlreadyExists, "entry already exists: "+name)
		}
	}
	if s.writer == nil {
		return model.RemoteEntry{}, errWritesNotWired
	}
	result, err := s.writer.Rename(entry.ID, name)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	s.invalidate()
	return result, nil
}

// MoveToParentPath moves an entry under a new parent, rejecting a move into
// itself or onto an existing name.
func (s *Storage) MoveToParentPath(path string, parentPath string) (model.RemoteEntry, error) {
	sourceParts, err := SplitRemotePath(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if len(sourceParts) == 0 {
		return model.RemoteEntry{}, model.NewStorageError(model.KindInvalidPath, "the root cannot be moved")
	}
	destinationParentParts, err := SplitRemotePath(parentPath)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if len(destinationParentParts) >= len(sourceParts) &&
		slices.Equal(destinationParentParts[:len(sourceParts)], sourceParts) {
		return model.RemoteEntry{}, model.NewStorageError(model.KindInvalidPath, "an entry cannot be moved into itself")
	}

	entry, err := s.Resolve(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	sourceParent, err := s.resolveParts(sourceParts[:len(sourceParts)-1])
	if err != nil {
		return model.RemoteEntry{}, err
	}
	destinationParent, err := s.Resolve(parentPath)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if destinationParent.Kind != model.KindFolder {
		return model.RemoteEntry{}, model.NewStorageError(model.KindNotFolder, "not a destination folder: "+parentPath)
	}
	if sourceParent.ID == destinationParent.ID {
		return entry, nil
	}
	children, err := s.children(destinationParent.ID)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	for _, sibling := range children {
		if sibling.Name == entry.Name {
			return model.RemoteEntry{}, model.NewStorageError(model.KindAlreadyExists, "entry already exists: "+entry.Name)
		}
	}
	if s.writer == nil {
		return model.RemoteEntry{}, errWritesNotWired
	}
	if err := s.writer.Move(entry.ID, sourceParent.ID, destinationParent.ID); err != nil {
		return model.RemoteEntry{}, err
	}
	s.invalidate()
	return model.RemoteEntry{
		ID:         entry.ID,
		Name:       entry.Name,
		Kind:       entry.Kind,
		ParentID:   model.Ptr(destinationParent.ID),
		Size:       entry.Size,
		ModifiedAt: entry.ModifiedAt,
		Etag:       entry.Etag,
		LinkID:     entry.LinkID,
	}, nil
}

// MovePath moves to a full destination path, preserving the entry name.
// Same-parent renames are served by RenamePath; cross-folder renames are
// unsupported exactly like the Python reference.
func (s *Storage) MovePath(path string, destinationPath string) (model.RemoteEntry, error) {
	sourceParts, err := SplitRemotePath(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	destinationParts, err := SplitRemotePath(destinationPath)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if len(sourceParts) == 0 || len(destinationParts) == 0 {
		return model.RemoteEntry{}, model.NewStorageError(model.KindInvalidPath, "the root cannot be moved")
	}
	entry, err := s.Resolve(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if destinationParts[len(destinationParts)-1] != entry.Name {
		if slices.Equal(sourceParts[:len(sourceParts)-1], destinationParts[:len(destinationParts)-1]) {
			return s.RenamePath(path, destinationParts[len(destinationParts)-1])
		}
		return model.RemoteEntry{}, model.NewStorageError(model.KindUnsupportedOperation, "cross-folder move with rename is not supported")
	}
	destinationParentPath, err := JoinRemotePath(destinationParts[:len(destinationParts)-1], false)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	return s.MoveToParentPath(path, destinationParentPath)
}
