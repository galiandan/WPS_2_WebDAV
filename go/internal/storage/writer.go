// The writer adapter binds the WPS write surface into Storage. Methods land
// with their own migration stages; until then they refuse with a fixed
// unsupported error that is distinct from errWritesNotWired (a Storage with
// no Writer at all).

package storage

import (
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/wps"
)

// errWriteNotImplemented refuses a write method whose WPS call has not been
// ported yet, so a partially wired Writer can never fake a mutation.
var errWriteNotImplemented = model.NewStorageError(model.KindUnsupportedOperation, "write operation is not implemented in this stage")

// wpsWriter adapts *wps.Client to the Writer interface.
type wpsWriter struct {
	client *wps.Client
}

// NewWriter returns the real WPS write surface for a client. Write methods
// that belong to later migration stages fail with a fixed unsupported error.
func NewWriter(client *wps.Client) Writer {
	return wpsWriter{client: client}
}

func (w wpsWriter) CreateFolder(parentID string, name string) (model.RemoteEntry, error) {
	return w.client.CreateFolder(parentID, name)
}

func (w wpsWriter) Upload(request UploadRequest) (model.RemoteEntry, error) {
	return model.RemoteEntry{}, errWriteNotImplemented
}

func (w wpsWriter) Delete(entryID string) error {
	return errWriteNotImplemented
}

func (w wpsWriter) Rename(entryID string, name string) (model.RemoteEntry, error) {
	return model.RemoteEntry{}, errWriteNotImplemented
}

func (w wpsWriter) Move(entryID string, sourceParentID string, destinationParentID string) error {
	return errWriteNotImplemented
}
