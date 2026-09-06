// The writer adapter binds the WPS write surface into Storage. Every method
// delegates to the ported client call; the upload flow still refuses inside
// the client until its object-storage half lands, so a partial flow can
// never fake a mutation.

package storage

import (
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/wps"
)

// wpsWriter adapts *wps.Client to the Writer interface.
type wpsWriter struct {
	client *wps.Client
}

// NewWriter returns the real WPS write surface for a client.
func NewWriter(client *wps.Client) Writer {
	return wpsWriter{client: client}
}

func (w wpsWriter) CreateFolder(parentID string, name string) (model.RemoteEntry, error) {
	return w.client.CreateFolder(parentID, name)
}

func (w wpsWriter) Upload(request UploadRequest) (model.RemoteEntry, error) {
	return w.client.Upload(wps.UploadRequest{
		ParentID:    request.ParentID,
		Name:        request.Name,
		Source:      request.Source,
		Size:        request.Size,
		ContentType: request.ContentType,
		CSRFToken:   request.CSRFToken,
		Overwrite:   request.Overwrite,
	})
}

func (w wpsWriter) Delete(entryID string) error {
	return w.client.Delete(entryID)
}

func (w wpsWriter) Rename(entryID string, name string) (model.RemoteEntry, error) {
	return w.client.Rename(entryID, name)
}

func (w wpsWriter) Move(entryID string, sourceParentID string, destinationParentID string) error {
	return w.client.Move(entryID, sourceParentID, destinationParentID)
}

// Copy delegates the optional native copy surface; *wpsWriter therefore
// satisfies storage.Copier and native single-file copies take the captured
// v3 batch endpoint.
func (w wpsWriter) Copy(fileID string, targetParentID string) (string, error) {
	return w.client.Copy(fileID, targetParentID)
}
