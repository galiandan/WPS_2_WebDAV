package storage

import (
	"context"
	"path"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

var (
	errBoundSourceChanged      = model.NewStorageError(model.KindAlreadyExists, "source changed since the task was queued")
	errBoundDestinationChanged = model.NewStorageError(model.KindAlreadyExists, "destination folder changed since the task was queued")
)

// BatchMetadata resolves virtual space roots to the actual destination folder
// identity used by mutations. Browser Metadata intentionally keeps its synthetic
// space IDs for navigation, which must not be persisted as mutation bindings.
func (m *MultiSpace) BatchMetadata(path string) (model.RemoteEntry, error) {
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
		return single.Metadata(path)
	}
	space, child, err := m.route(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	return space.Metadata(child)
}

// ApplyBoundBatch binds delayed work to the original source and destination
// IDs. The ID used by the mutation is checked inside the same resolution path,
// so a replacement at the old name cannot accidentally become its target.
func (s *Storage) ApplyBoundBatch(ctx context.Context, operation, source, destination, sourceID, parentID string) error {
	if sourceID == "" {
		return model.NewStorageError(model.KindBadRequest, "task source identity is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.InvalidateMetadataCache()
	switch operation {
	case "delete":
		return s.deletePathWithID(source, sourceID)
	case "move", "copy":
		if parentID == "" {
			return model.NewStorageError(model.KindBadRequest, "task destination identity is required")
		}
		if _, err := SplitRemotePath(destination); err != nil {
			return err
		}
		if operation == "copy" {
			_, err := s.CopyPath(ctx, source, destination, CopyOptions{Depth: "infinity", ExpectedSourceID: sourceID, ExpectedParentID: parentID})
			return err
		}
		if path.Base(source) != path.Base(destination) {
			return model.NewStorageError(model.KindUnsupportedOperation, "task move cannot rename the source")
		}
		_, err := s.moveToParentWithIDs(source, path.Dir(destination), sourceID, parentID)
		return err
	default:
		return model.NewStorageError(model.KindBadRequest, "unknown task operation")
	}
}

func (m *MultiSpace) ApplyBoundBatch(ctx context.Context, operation, source, destination, sourceID, parentID string) error {
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
		return single.ApplyBoundBatch(ctx, operation, source, destination, sourceID, parentID)
	}
	sourceSpace, sourceChild, err := m.route(source)
	if err != nil {
		return err
	}
	destinationChild := ""
	if operation != "delete" {
		var destinationSpace *Storage
		destinationSpace, destinationChild, err = m.route(destination)
		if err != nil {
			return err
		}
		if sourceSpace != destinationSpace {
			return model.NewStorageError(model.KindUnsupportedOperation, "cross-space task operations are not supported")
		}
	}
	return sourceSpace.ApplyBoundBatch(ctx, operation, sourceChild, destinationChild, sourceID, parentID)
}
