package httpserver

import (
	"context"
	"net/http"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

type davLockPathMapper interface {
	LockPath(string) (string, error)
}

func (d *DAVDispatcher) lockPath(path string) (string, error) {
	if mapper, ok := d.storage.(davLockPathMapper); ok {
		mapped, err := mapper.LockPath(path)
		if err != nil {
			return "", err
		}
		return canonicalRemotePath(mapped)
	}
	return canonicalRemotePath(path)
}

func (d *DAVDispatcher) checkLocks(w http.ResponseWriter, r *http.Request, paths ...string) (bool, error) {
	mapped := make([]string, 0, len(paths))
	for _, path := range paths {
		canonical, err := d.lockPath(path)
		if err != nil {
			return false, err
		}
		mapped = append(mapped, canonical)
	}
	return checkLocks(w, r, d.locks, false, mapped...)
}

// Storage and HTTP interfaces deliberately use separately named stream types.
type scopedDAVDownloads struct{ *storage.DAVView }

func (d scopedDAVDownloads) OpenPath(ctx context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	return d.DAVView.OpenPath(ctx, path, offset, length)
}
