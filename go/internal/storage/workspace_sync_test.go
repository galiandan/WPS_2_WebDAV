package storage_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	storagepkg "github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/wps"
)

// listStub is the minimal read surface for the workspace-sync tests; the
// internal fake lives in the package-internal test file.
type listStub struct {
	listCalls []string
}

func (l *listStub) IterEntries(parentID string, _ wps.IterOptions) ([]model.RemoteEntry, error) {
	l.listCalls = append(l.listCalls, parentID)
	return []model.RemoteEntry{
		{ID: "child", Name: "child.txt", Kind: model.KindFile, ParentID: model.Ptr(parentID), Size: model.Ptr(int64(1))},
	}, nil
}

// privateTempDir returns a 0700 directory for workspace files, matching the
// securefile privacy requirement.
func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newSyncStorage(t *testing.T, state *workspace.WorkspaceState, stub *listStub) *storagepkg.Storage {
	t.Helper()
	transferBudget, err := budget.New(budget.Config{MaxUploads: 1, MaxDownloads: 1, MaxConnections: 4, TransferWaitTimeout: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	config := storagepkg.DefaultStorageConfig("0")
	config.CacheTTLSeconds = 60
	config.WorkspaceSelection = func() (string, string, bool, error) {
		groupID, err := state.GroupID()
		if err != nil {
			return "", "", false, err
		}
		rootID, err := state.RootID()
		if err != nil {
			return "", "", false, err
		}
		return groupID, rootID, state.ConfiguredRootID() == "auto", nil
	}
	storage, err := storagepkg.NewStorage(stub, transferBudget, config)
	if err != nil {
		t.Fatal(err)
	}
	return storage
}

// TestAutoWorkspaceRootIsReloadedAfterLoginSelection mirrors
// test_auto_workspace_root_is_reloaded_after_login_selection: a login-side
// workspace.update is picked up by the storage's virtual root.
func TestAutoWorkspaceRootIsReloadedAfterLoginSelection(t *testing.T) {
	state, err := workspace.NewWorkspaceState(filepath.Join(privateTempDir(t), "workspace.json"), "auto", "auto")
	if err != nil {
		t.Fatal(err)
	}
	storage := newSyncStorage(t, state, &listStub{})

	if err := state.Update("group", "selected-root", nil); err != nil {
		t.Fatal(err)
	}
	root, err := storage.Root()
	if err != nil {
		t.Fatal(err)
	}
	if root.ID != "selected-root" {
		t.Fatalf("root id = %q, want selected-root", root.ID)
	}
}

// TestWorkspaceGroupChangeInvalidatesMetadataCache mirrors
// test_workspace_group_change_invalidates_metadata_cache: a group change
// clears the metadata cache even when the root id stays the same.
func TestWorkspaceGroupChangeInvalidatesMetadataCache(t *testing.T) {
	state, err := workspace.NewWorkspaceState(filepath.Join(privateTempDir(t), "workspace.json"), "auto", "auto")
	if err != nil {
		t.Fatal(err)
	}
	stub := &listStub{}
	storage := newSyncStorage(t, state, stub)

	if err := state.Update("group-1", "root-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ListPath("/"); err != nil {
		t.Fatal(err)
	}
	if len(stub.listCalls) != 1 {
		t.Fatalf("list calls after first listing = %v", stub.listCalls)
	}
	// Group change only: the root id stays root-1, but the cache must go.
	if err := state.Update("group-2", "root-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Root(); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ListPath("/"); err != nil {
		t.Fatal(err)
	}
	if len(stub.listCalls) != 2 {
		t.Fatalf("cache survived the group change: %v", stub.listCalls)
	}
}

// TestFixedRootDoesNotFollowWorkspace pins the auto-only rule: a storage
// whose selection reports a non-auto configured root never syncs.
func TestFixedRootDoesNotFollowWorkspace(t *testing.T) {
	state, err := workspace.NewWorkspaceState(filepath.Join(privateTempDir(t), "workspace.json"), "auto", "0")
	if err != nil {
		t.Fatal(err)
	}
	stub := &listStub{}
	storage := newSyncStorage(t, state, stub)

	if err := state.Update("group-1", "root-1", nil); err != nil {
		t.Fatal(err)
	}
	root, err := storage.Root()
	if err != nil {
		t.Fatal(err)
	}
	if root.ID != "0" {
		t.Fatalf("fixed root followed the selection: %q", root.ID)
	}
	if _, err := storage.ListPath("/"); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Root(); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ListPath("/"); err != nil {
		t.Fatal(err)
	}
	if len(stub.listCalls) != 1 {
		t.Fatalf("cache did not persist without remaps: %v", stub.listCalls)
	}
}

// TestWorkspaceErrorsPropagate keeps the read paths honest when the
// selection cannot be loaded.
func TestWorkspaceErrorsPropagate(t *testing.T) {
	transferBudget, err := budget.New(budget.Config{MaxUploads: 1, MaxDownloads: 1, MaxConnections: 4, TransferWaitTimeout: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	config := storagepkg.DefaultStorageConfig("0")
	config.WorkspaceSelection = func() (string, string, bool, error) {
		return "", "", false, errors.New("workspace file is unreadable")
	}
	storage, err := storagepkg.NewStorage(&listStub{}, transferBudget, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Root(); err == nil || err.Error() != "workspace file is unreadable" {
		t.Fatalf("root error = %v", err)
	}
	if _, err := storage.ListPath("/"); err == nil || err.Error() != "workspace file is unreadable" {
		t.Fatalf("list error = %v", err)
	}
}
