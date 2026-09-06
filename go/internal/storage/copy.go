// The COPY surface ports storage.py's copy_path: a native same-group WPS
// batch copy for the one shape the captured endpoint accepts, and a bounded
// streaming download/upload relay for everything else. File bytes are never
// held in memory as a whole and the destination tree is never overwritten —
// an existing destination is refused before any WPS mutation.

package storage

import (
	"context"
	"io"
	"slices"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/mimetypes"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// Copier is the optional native same-group file-copy surface. Python looks
// the method up with getattr(self.client, "copy", None); a Writer without
// Copy simply never takes the native branch and every copy relays.
type Copier interface {
	Copy(fileID string, targetParentID string) (string, error)
}

// CopyOptions carries copy_path's keyword surface. Depth is validated
// exactly like the Python str.strip().lower() gate.
type CopyOptions struct {
	Depth     string
	Overwrite bool
}

// CopyPath mirrors copy_path: depth validation, self-copy and folder-into-
// itself refusals, an existing-destination refusal that never deletes the
// target, the native single-file branch, and the recursive relay.
func (s *Storage) CopyPath(ctx context.Context, sourcePath string, destinationPath string, options CopyOptions) (model.RemoteEntry, error) {
	depth := strings.TrimSpace(strings.ToLower(options.Depth))
	if depth != "0" && depth != "1" && depth != "infinity" {
		return model.RemoteEntry{}, model.NewStorageError(model.KindInvalidPath, "COPY Depth must be 0, 1 or infinity")
	}
	sourceParts, err := SplitRemotePath(sourcePath)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	destinationParts, err := SplitRemotePath(destinationPath)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if len(sourceParts) == 0 || len(destinationParts) == 0 {
		return model.RemoteEntry{}, model.NewStorageError(model.KindInvalidPath, "the root cannot be copied")
	}
	if slices.Equal(sourceParts, destinationParts) {
		return model.RemoteEntry{}, model.NewStorageError(model.KindInvalidPath, "an entry cannot be copied onto itself")
	}
	source, err := s.resolveParts(sourceParts)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if source.Kind == model.KindFolder && len(destinationParts) >= len(sourceParts) &&
		slices.Equal(destinationParts[:len(sourceParts)], sourceParts) {
		return model.RemoteEntry{}, model.NewStorageError(model.KindInvalidPath, "a folder cannot be copied into itself")
	}
	destinationParent, err := s.resolveParts(destinationParts[:len(destinationParts)-1])
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if destinationParent.Kind != model.KindFolder {
		return model.RemoteEntry{}, model.NewStorageError(model.KindNotFolder, "the COPY destination parent is not a folder")
	}
	destinationName := destinationParts[len(destinationParts)-1]
	if err := validateEntryName(destinationName); err != nil {
		return model.RemoteEntry{}, err
	}

	var existing *model.RemoteEntry
	entry, err := s.Resolve(destinationPath)
	switch {
	case err == nil:
		existing = &entry
	case isEntryNotFound(err):
	default:
		return model.RemoteEntry{}, err
	}
	// The captured WPS COPY API accepts only a destination parent and always
	// preserves the source name, so a renamed destination would report a
	// path that does not exist; the relay refuses the same shapes.
	if existing != nil {
		if !options.Overwrite {
			return model.RemoteEntry{}, model.NewStorageError(model.KindAlreadyExists, "entry already exists: "+destinationPath)
		}
		return model.RemoteEntry{}, model.NewStorageError(model.KindUnsupportedOperation, "COPY overwrite is disabled because the relay is not atomic")
	}

	// The captured endpoint only fits a file whose basename is unchanged.
	if source.Kind == model.KindFile && destinationName == source.Name {
		if copier, ok := s.writer.(Copier); ok {
			copiedID, err := copier.Copy(source.ID, destinationParent.ID)
			if err != nil {
				return model.RemoteEntry{}, err
			}
			s.invalidate()
			return model.RemoteEntry{
				ID:         copiedID,
				Name:       destinationName,
				Kind:       model.KindFile,
				ParentID:   model.Ptr(destinationParent.ID),
				Size:       source.Size,
				ModifiedAt: source.ModifiedAt,
				Etag:       source.Etag,
				LinkID:     source.LinkID,
			}, nil
		}
	}

	copied := 0
	var copyEntry func(sourceEntry model.RemoteEntry, sourceParts []string, destinationParentEntry model.RemoteEntry, destinationItemName string, level int) (model.RemoteEntry, error)
	copyEntry = func(sourceEntry model.RemoteEntry, sourceParts []string, destinationParentEntry model.RemoteEntry, destinationItemName string, level int) (model.RemoteEntry, error) {
		copied++
		if copied > s.maxCopyEntries {
			return model.RemoteEntry{}, model.NewStorageError(model.KindInsufficientStorage, "COPY exceeds the configured entry limit")
		}
		if level > s.maxCopyDepth {
			return model.RemoteEntry{}, model.NewStorageError(model.KindInsufficientStorage, "COPY exceeds the configured depth limit")
		}

		if sourceEntry.Kind == model.KindFile {
			sourceItemPath, err := JoinRemotePath(sourceParts, false)
			if err != nil {
				return model.RemoteEntry{}, err
			}
			// The download slot is held for the whole stream; the upload
			// slot is taken while it is held. Both waits are bounded by the
			// transfer budget and the pools are disjoint, so no relay can
			// deadlock against a mirrored acquisition order.
			stream, err := s.OpenPath(ctx, sourceItemPath, 0, nil)
			if err != nil {
				return model.RemoteEntry{}, err
			}
			defer stream.Close()
			// Cancellation closes the stream immediately, which unblocks a
			// pending read and tears down both slots; the managed close is
			// idempotent.
			watcherDone := make(chan struct{})
			defer close(watcherDone)
			go func() {
				select {
				case <-ctx.Done():
					stream.Close()
				case <-watcherDone:
				}
			}()
			result, err := s.uploadStream(ctx, destinationParentEntry.ID, destinationItemName, stream,
				sourceEntry.Size, mimetypes.GuessMimeType(sourceEntry.Name), false)
			if err != nil {
				return model.RemoteEntry{}, err
			}
			s.invalidate()
			return result, nil
		}

		// Python calls client.create_folder directly here — no collision
		// check, because the top-level gate already refused an existing
		// destination and children are created fresh.
		if s.writer == nil {
			return model.RemoteEntry{}, errWritesNotWired
		}
		result, err := s.writer.CreateFolder(destinationParentEntry.ID, destinationItemName)
		if err != nil {
			return model.RemoteEntry{}, err
		}
		s.invalidate()
		if depth == "0" || (depth == "1" && level >= 1) {
			return result, nil
		}
		children, listErr := s.children(sourceEntry.ID)
		if listErr != nil {
			s.cleanupCopyFolder(result.ID)
			return model.RemoteEntry{}, listErr
		}
		for _, childEntry := range children {
			_, err := copyEntry(childEntry, slices.Concat(sourceParts, []string{childEntry.Name}), result, childEntry.Name, level+1)
			if err != nil {
				// The destination did not exist before this request. Remove
				// the newly-created folder when the recursive relay fails
				// part-way through; a failed cleanup is swallowed.
				s.cleanupCopyFolder(result.ID)
				return model.RemoteEntry{}, err
			}
		}
		return result, nil
	}

	return copyEntry(source, sourceParts, destinationParent, destinationName, 0)
}

// uploadStream mirrors _upload_stream: the upload slot is held for the
// writer call exactly like the path-based upload route.
func (s *Storage) uploadStream(ctx context.Context, parentID string, name string, source io.Reader, size *int64, contentType string, overwrite bool) (model.RemoteEntry, error) {
	release, err := s.budget.AcquireUpload(ctx)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	defer release()
	if s.writer == nil {
		return model.RemoteEntry{}, errWritesNotWired
	}
	return s.writer.Upload(UploadRequest{
		ParentID:    parentID,
		Name:        name,
		Source:      source,
		Size:        size,
		ContentType: contentType,
		Overwrite:   overwrite,
	})
}

// cleanupCopyFolder removes a folder this copy created; every failure is
// swallowed and the cache is dropped regardless, mirroring the Python
// best-effort delete.
func (s *Storage) cleanupCopyFolder(folderID string) {
	if s.writer != nil {
		_ = s.writer.Delete(folderID)
	}
	s.invalidate()
}

func isEntryNotFound(err error) bool {
	storageErr, ok := model.AsStorageError(err)
	return ok && storageErr.Kind == model.KindEntryNotFound
}
