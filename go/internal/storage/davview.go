package storage

import (
	"context"
	"errors"
	"io"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// DAVView exposes one dynamically selected subtree of a MultiSpace as the
// WebDAV root. The browser-facing MultiSpace keeps all selected WPS spaces;
// this view only prefixes DAV paths with the one configured WebDAV location.
// The prefix is resolved for every operation so a web settings change takes
// effect without restarting the service.
type DAVView struct {
	base   *MultiSpace
	prefix func() (string, error)
}

// NewDAVView validates and builds a WebDAV subtree view.
func NewDAVView(base *MultiSpace, prefix func() (string, error)) (*DAVView, error) {
	if base == nil {
		return nil, errors.New("a multi-space storage is required")
	}
	if prefix == nil {
		return nil, errors.New("a DAV prefix source is required")
	}
	return &DAVView{base: base, prefix: prefix}, nil
}

// mapPath converts a DAV-relative path such as /test.txt into the current
// browser namespace path such as /A/web/test.txt.
func (v *DAVView) mapPath(path string) (string, error) {
	pathParts, err := SplitRemotePath(path)
	if err != nil {
		return "", err
	}
	prefix, err := v.prefix()
	if err != nil {
		return "", err
	}
	prefixParts, err := SplitRemotePath(prefix)
	if err != nil {
		return "", err
	}
	return JoinRemotePath(append(prefixParts, pathParts...), false)
}

func (v *DAVView) Metadata(path string) (model.RemoteEntry, error) {
	mapped, err := v.mapPath(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	return v.base.Metadata(mapped)
}

func (v *DAVView) ListPath(path string) ([]model.RemoteEntry, error) {
	mapped, err := v.mapPath(path)
	if err != nil {
		return nil, err
	}
	return v.base.ListPath(mapped)
}

func (v *DAVView) ListChildren(scopePath string, entry model.RemoteEntry) ([]model.RemoteEntry, error) {
	mapped, err := v.mapPath(scopePath)
	if err != nil {
		return nil, err
	}
	return v.base.ListChildren(mapped, entry)
}

func (v *DAVView) OpenPath(ctx context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	mapped, err := v.mapPath(path)
	if err != nil {
		return nil, err
	}
	return v.base.OpenPath(ctx, mapped, offset, length)
}

func (v *DAVView) UploadPath(ctx context.Context, path string, source io.Reader, options UploadOptions) (model.RemoteEntry, error) {
	mapped, err := v.mapPath(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	return v.base.UploadPath(ctx, mapped, source, options)
}

func (v *DAVView) CreateFolderPath(path string) (model.RemoteEntry, error) {
	mapped, err := v.mapPath(path)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	return v.base.CreateFolderPath(mapped)
}

func (v *DAVView) DeletePath(path string) error {
	mapped, err := v.mapPath(path)
	if err != nil {
		return err
	}
	return v.base.DeletePath(mapped)
}

func (v *DAVView) MovePath(source string, destination string) (model.RemoteEntry, error) {
	mappedSource, err := v.mapPath(source)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	mappedDestination, err := v.mapPath(destination)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	return v.base.MovePath(mappedSource, mappedDestination)
}

func (v *DAVView) CopyPath(ctx context.Context, source string, destination string, options CopyOptions) (model.RemoteEntry, error) {
	mappedSource, err := v.mapPath(source)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	mappedDestination, err := v.mapPath(destination)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	return v.base.CopyPath(ctx, mappedSource, mappedDestination, options)
}
