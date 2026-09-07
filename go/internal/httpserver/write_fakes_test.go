package httpserver

import (
	"context"
	"errors"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

// stubMutations refuses every write; read-only route tests wire it so an
// unexpected mutation call fails loudly. It satisfies both the DAV and the
// REST mutation interfaces.
type stubMutations struct{}

func (stubMutations) CreateFolderPath(path string) (model.RemoteEntry, error) {
	return model.RemoteEntry{}, errors.New("unexpected CreateFolderPath call")
}

func (stubMutations) DeletePath(path string) error {
	return errors.New("unexpected DeletePath call")
}

func (stubMutations) RenamePath(path string, name string) (model.RemoteEntry, error) {
	return model.RemoteEntry{}, errors.New("unexpected RenamePath call")
}

func (stubMutations) MovePath(path string, destination string) (model.RemoteEntry, error) {
	return model.RemoteEntry{}, errors.New("unexpected MovePath call")
}

func (stubMutations) MoveToParentPath(path string, parentPath string) (model.RemoteEntry, error) {
	return model.RemoteEntry{}, errors.New("unexpected MoveToParentPath call")
}

func (stubMutations) CopyPath(ctx context.Context, source string, destination string, options storage.CopyOptions) (model.RemoteEntry, error) {
	return model.RemoteEntry{}, errors.New("unexpected CopyPath call")
}

// newTestLockStore builds the dispatcher default lock store.
func newTestLockStore(t *testing.T) *DavLockStore {
	t.Helper()
	store, err := NewDavLockStore(DefaultLockMaxTimeout, DefaultLockMaxLocks)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
