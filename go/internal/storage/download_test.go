// The download adapter pins the wiring between the signed-object client
// and the storage Downloader surface.

package storage

// NewDownloader(nil) compiles only while *wps.Client's OpenDownload return
// value keeps satisfying the storage DownloadStream interface.
var _ Downloader = NewDownloader(nil)
