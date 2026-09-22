package storage

import (
	"context"
	"errors"
	"io"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// ScopedStorage is the path-only surface used by a member's fixed directory
// view. No method accepting arbitrary WPS IDs is exposed by Scoped.
type ScopedStorage interface {
	Metadata(string) (model.RemoteEntry, error)
	ListPath(string) ([]model.RemoteEntry, error)
	OpenPath(context.Context, string, int64, *int64) (DownloadStream, error)
	UploadPath(context.Context, string, io.Reader, UploadOptions) (model.RemoteEntry, error)
	CreateFolderPath(string) (model.RemoteEntry, error)
	DeletePath(string) error
	RenamePath(string, string) (model.RemoteEntry, error)
	MovePath(string, string) (model.RemoteEntry, error)
	MoveToParentPath(string, string) (model.RemoteEntry, error)
	CopyPath(context.Context, string, string, CopyOptions) (model.RemoteEntry, error)
	InvalidateMetadataCache()
}

type ScopedConfig struct {
	RootPath, RootID, RootName string
	Binding                    string
	Read, Upload, Delete       bool
	// Validate rechecks active principal/policy state before every operation.
	// Use it to revoke delayed tasks and already-created per-user dispatchers.
	Validate func() error
}

// Scoped maps visible paths below one immutable configured directory. The
// expected root ID is rechecked with uncached metadata for every operation;
// renaming or replacing the pinned root fails closed. This is an adapter-side
// boundary: WPS has no atomic path/permission transaction across a remote call.
type Scoped struct {
	base      ScopedStorage
	config    ScopedConfig
	rootParts []string
}

func NewScoped(base ScopedStorage, config ScopedConfig) (*Scoped, error) {
	if base == nil || config.RootID == "" {
		return nil, errors.New("scoped storage and root identity are required")
	}
	parts, err := SplitRemotePath(config.RootPath)
	if err != nil {
		return nil, err
	}
	config.RootPath, _ = JoinRemotePath(parts, false)
	if config.RootName == "" {
		config.RootName = "我的文件"
	}
	if _, err := JoinRemotePath([]string{config.RootName}, false); err != nil {
		return nil, err
	}
	return &Scoped{base: base, config: config, rootParts: parts}, nil
}

func scopedDenied() error { return model.NewStorageError(model.KindPermissionDenied, "access denied") }

// Error messages from the backing view can contain its full business path.
// Retain useful categories while replacing all path-bearing descriptions.
func scopedError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, ErrUploadTargetChanged) {
		return ErrUploadTargetChanged
	}
	if storageErr, ok := model.AsStorageError(err); ok {
		messages := map[model.ErrorKind]string{
			model.KindInvalidPath: "invalid path", model.KindEntryNotFound: "entry not found",
			model.KindNotFolder: "requested entry is not a folder", model.KindAlreadyExists: "destination already exists or entry changed",
			model.KindInsufficientStorage: "storage limit exceeded", model.KindServiceBusy: "storage is busy",
			model.KindAmbiguousPath: "path is ambiguous", model.KindUnsupportedOperation: "operation is unavailable",
			model.KindIOFailure: "storage operation failed", model.KindBadRequest: "invalid storage request",
			model.KindPermissionDenied: "access denied",
		}
		message := messages[storageErr.Kind]
		if message == "" {
			return model.NewStorageError(model.KindIOFailure, "storage operation failed")
		}
		return model.NewStorageError(storageErr.Kind, message)
	}
	if upstream, ok := model.AsWpsAPIError(err); ok {
		return model.NewWpsAPIError("scoped storage operation", upstream.Status, upstream.Category)
	}
	return model.NewStorageError(model.KindIOFailure, "storage operation failed")
}

func (s *Scoped) mapped(path string, mutate bool) (string, error) {
	parts, err := SplitRemotePath(path)
	if err != nil {
		return "", scopedError(err)
	}
	if mutate && len(parts) == 0 {
		return "", model.NewStorageError(model.KindInvalidPath, "the root cannot be changed")
	}
	if s.config.Validate != nil && s.config.Validate() != nil {
		return "", scopedDenied()
	}
	if err := s.checkBinding(); err != nil {
		return "", err
	}
	s.base.InvalidateMetadataCache()
	root, err := s.rootMetadata()
	if err != nil || root.Kind != model.KindFolder || root.ID != s.config.RootID {
		return "", model.NewStorageError(model.KindPermissionDenied, "configured root is unavailable or has changed")
	}
	if err := s.checkBinding(); err != nil {
		return "", err
	}
	if s.config.Validate != nil && s.config.Validate() != nil {
		return "", scopedDenied()
	}
	joined := make([]string, 0, len(s.rootParts)+len(parts))
	joined = append(joined, s.rootParts...)
	joined = append(joined, parts...)
	return JoinRemotePath(joined, false)
}

func (s *Scoped) checkBinding() error {
	if s.config.Binding == "" {
		return nil
	}
	source, ok := s.base.(interface{ ScopeBinding(string) (string, error) })
	if !ok {
		return scopedDenied()
	}
	current, err := source.ScopeBinding(s.config.RootPath)
	if err != nil || current != s.config.Binding {
		return scopedDenied()
	}
	return nil
}

func (s *Scoped) rootMetadata() (model.RemoteEntry, error) {
	if concrete, ok := s.base.(interface {
		BatchMetadata(string) (model.RemoteEntry, error)
	}); ok {
		return concrete.BatchMetadata(s.config.RootPath)
	}
	return s.base.Metadata(s.config.RootPath)
}

func (s *Scoped) public(entry model.RemoteEntry, path string) model.RemoteEntry {
	entry.ParentID, entry.LinkID = nil, nil
	if path == "/" {
		entry.Name = s.config.RootName
		entry.ID = s.config.RootID
	}
	return entry
}

func (s *Scoped) Metadata(path string) (model.RemoteEntry, error) {
	if !s.config.Read {
		return model.RemoteEntry{}, scopedDenied()
	}
	mapped, err := s.mapped(path, false)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	entry, err := s.base.Metadata(mapped)
	if err != nil {
		return model.RemoteEntry{}, scopedError(err)
	}
	return s.public(entry, path), nil
}

func (s *Scoped) ListPath(path string) ([]model.RemoteEntry, error) {
	if !s.config.Read {
		return nil, scopedDenied()
	}
	mapped, err := s.mapped(path, false)
	if err != nil {
		return nil, err
	}
	entries, err := s.base.ListPath(mapped)
	if err != nil {
		return nil, scopedError(err)
	}
	result := make([]model.RemoteEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, s.public(entry, ""))
	}
	return result, nil
}

// Traversal callers may carry an older entry or a forged ID. Resolve only the
// validated visible path, never the caller's ID, before listing its children.
func (s *Scoped) ListChildren(path string, _ model.RemoteEntry) ([]model.RemoteEntry, error) {
	return s.ListPath(path)
}

func (s *Scoped) OpenPath(ctx context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	if !s.config.Read {
		return nil, scopedDenied()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mapped, err := s.mapped(path, false)
	if err != nil {
		return nil, err
	}
	stream, err := s.base.OpenPath(ctx, mapped, offset, length)
	return stream, scopedError(err)
}

func (s *Scoped) UploadPath(ctx context.Context, path string, source io.Reader, options UploadOptions) (model.RemoteEntry, error) {
	if !s.config.Upload {
		return model.RemoteEntry{}, scopedDenied()
	}
	if err := ctx.Err(); err != nil {
		return model.RemoteEntry{}, err
	}
	mapped, err := s.mapped(path, true)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if !s.config.Delete {
		// DAV PUT normally requests overwrite even for a brand-new file. A
		// member with upload only can create it, but must retain no-overwrite
		// semantics through the backing storage's final collision check.
		if options.ExpectedID != "" {
			return model.RemoteEntry{}, scopedDenied()
		}
		if options.Overwrite {
			if _, err := s.base.Metadata(mapped); err == nil {
				return model.RemoteEntry{}, scopedDenied()
			} else if !scopedNotFound(err) {
				return model.RemoteEntry{}, scopedError(err)
			}
		}
		options.Overwrite = false
	}
	entry, err := s.base.UploadPath(ctx, mapped, source, options)
	return s.public(entry, ""), scopedError(err)
}

func scopedNotFound(err error) bool {
	storageErr, ok := model.AsStorageError(err)
	return ok && storageErr.Kind == model.KindEntryNotFound
}

func (s *Scoped) CreateFolderPath(path string) (model.RemoteEntry, error) {
	if !s.config.Upload {
		return model.RemoteEntry{}, scopedDenied()
	}
	mapped, err := s.mapped(path, true)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	entry, err := s.base.CreateFolderPath(mapped)
	return s.public(entry, ""), scopedError(err)
}

func (s *Scoped) DeletePath(path string) error {
	if !s.config.Delete {
		return scopedDenied()
	}
	mapped, err := s.mapped(path, true)
	if err != nil {
		return err
	}
	return scopedError(s.base.DeletePath(mapped))
}

func (s *Scoped) RenamePath(path, name string) (model.RemoteEntry, error) {
	if !s.config.Upload || !s.config.Delete {
		return model.RemoteEntry{}, scopedDenied()
	}
	if _, err := JoinRemotePath([]string{name}, false); err != nil {
		return model.RemoteEntry{}, scopedError(err)
	}
	mapped, err := s.mapped(path, true)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	entry, err := s.base.RenamePath(mapped, name)
	return s.public(entry, ""), scopedError(err)
}

func (s *Scoped) MovePath(source, destination string) (model.RemoteEntry, error) {
	return s.move(source, destination, false)
}

func (s *Scoped) MoveToParentPath(source, parent string) (model.RemoteEntry, error) {
	return s.move(source, parent, true)
}

func (s *Scoped) move(source, destination string, parent bool) (model.RemoteEntry, error) {
	if !s.config.Upload || !s.config.Delete {
		return model.RemoteEntry{}, scopedDenied()
	}
	from, err := s.mapped(source, true)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	to, err := s.mapped(destination, !parent)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	var entry model.RemoteEntry
	if parent {
		entry, err = s.base.MoveToParentPath(from, to)
	} else {
		entry, err = s.base.MovePath(from, to)
	}
	return s.public(entry, ""), scopedError(err)
}

func (s *Scoped) CopyPath(ctx context.Context, source, destination string, options CopyOptions) (model.RemoteEntry, error) {
	if !s.config.Read || !s.config.Upload {
		return model.RemoteEntry{}, scopedDenied()
	}
	if err := ctx.Err(); err != nil {
		return model.RemoteEntry{}, err
	}
	from, err := s.mapped(source, true)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	to, err := s.mapped(destination, true)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if options.Overwrite && !s.config.Delete {
		options.Overwrite = false
	}
	entry, err := s.base.CopyPath(ctx, from, to, options)
	return s.public(entry, ""), scopedError(err)
}

func (s *Scoped) BatchMetadata(path string) (model.RemoteEntry, error) {
	return s.Metadata(path)
}

// CheckBatchPermission rejects unauthorized queued work before it consumes a
// process-wide task slot. Execute paths call it again to catch revocation.
func (s *Scoped) CheckBatchPermission(operation string) error {
	if s.config.Validate != nil && s.config.Validate() != nil {
		return scopedDenied()
	}
	switch operation {
	case "delete":
		if !s.config.Delete {
			return scopedDenied()
		}
	case "move":
		if !s.config.Upload || !s.config.Delete {
			return scopedDenied()
		}
	case "copy":
		if !s.config.Read || !s.config.Upload {
			return scopedDenied()
		}
	default:
		return model.NewStorageError(model.KindBadRequest, "unknown operation")
	}
	return nil
}

func (s *Scoped) ApplyBoundBatch(ctx context.Context, operation, source, destination, sourceID, parentID string) error {
	if err := s.CheckBatchPermission(operation); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	from, err := s.mapped(source, true)
	if err != nil {
		return err
	}
	to := ""
	if operation != "delete" {
		to, err = s.mapped(destination, true)
		if err != nil {
			return err
		}
	}
	bound, ok := s.base.(interface {
		ApplyBoundBatch(context.Context, string, string, string, string, string) error
	})
	if !ok {
		return model.NewStorageError(model.KindUnsupportedOperation, "background operations are unavailable")
	}
	return scopedError(bound.ApplyBoundBatch(ctx, operation, from, to, sourceID, parentID))
}

func (s *Scoped) LockPath(path string) (string, error) { return s.mapped(path, false) }

func (s *Scoped) InvalidateMetadataCache() { s.base.InvalidateMetadataCache() }
