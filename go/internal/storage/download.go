// The download adapter binds the signed-object surface of *wps.Client to
// the storage Downloader interface so the HTTP layer can open streams
// through the storage facade.

package storage

import "github.com/galiandan/WPS_2_WebDAV/go/internal/wps"

// wpsDownloader adapts *wps.Client's OpenDownload return type to the
// storage DownloadStream interface.
type wpsDownloader struct {
	client *wps.Client
}

func (d *wpsDownloader) OpenDownload(entryID string, offset int64, length *int64, cid *string) (DownloadStream, error) {
	return d.client.OpenDownload(entryID, offset, length, cid)
}

// NewDownloader returns the real WPS download surface for a client.
func NewDownloader(client *wps.Client) Downloader {
	return &wpsDownloader{client: client}
}
