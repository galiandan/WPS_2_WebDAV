package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/wps"
)

// fakeClient mirrors tests/test_storage.py's FakeClient: an in-memory
// children table plus recorded write calls.
type fakeClient struct {
	children map[string][]model.RemoteEntry

	listCalls     []string
	uploadCalls   []fakeUpload
	downloadCalls []fakeDownload
	folderCalls   [][2]string
	deleteCalls   []string
	renameCalls   [][2]string
	moveCalls     [][3]string

	uploadErr   error
	downloadErr error
	stream      DownloadStream
}

type fakeUpload struct {
	parentID string
	name     string
	body     []byte
	request  UploadRequest
}

type fakeDownload struct {
	entryID string
	offset  int64
	length  *int64
	cid     *string
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		children: map[string][]model.RemoteEntry{
			"root": {
				{ID: "docs", Name: "docs", Kind: model.KindFolder, ParentID: model.Ptr("root"), Size: model.Ptr(int64(0))},
				{ID: "top", Name: "top.txt", Kind: model.KindFile, ParentID: model.Ptr("root"), Size: model.Ptr(int64(3))},
			},
			"docs": {
				{ID: "readme", Name: "readme.txt", Kind: model.KindFile, ParentID: model.Ptr("docs"), Size: model.Ptr(int64(4))},
			},
		},
	}
}

func (f *fakeClient) IterEntries(parentID string, _ wps.IterOptions) ([]model.RemoteEntry, error) {
	f.listCalls = append(f.listCalls, parentID)
	return f.children[parentID], nil
}

func (f *fakeClient) Upload(request UploadRequest) (model.RemoteEntry, error) {
	if f.uploadErr != nil {
		return model.RemoteEntry{}, f.uploadErr
	}
	body, _ := io.ReadAll(request.Source)
	f.uploadCalls = append(f.uploadCalls, fakeUpload{parentID: request.ParentID, name: request.Name, body: body, request: request})
	return model.RemoteEntry{ID: "new", Name: request.Name, Kind: model.KindFile, ParentID: model.Ptr(request.ParentID), Size: model.Ptr(int64(len(body)))}, nil
}

func (f *fakeClient) CreateFolder(parentID string, name string) (model.RemoteEntry, error) {
	f.folderCalls = append(f.folderCalls, [2]string{parentID, name})
	return model.RemoteEntry{ID: "folder-new", Name: name, Kind: model.KindFolder, ParentID: model.Ptr(parentID), Size: model.Ptr(int64(0))}, nil
}

func (f *fakeClient) Delete(entryID string) error {
	f.deleteCalls = append(f.deleteCalls, entryID)
	return nil
}

func (f *fakeClient) Rename(entryID string, name string) (model.RemoteEntry, error) {
	f.renameCalls = append(f.renameCalls, [2]string{entryID, name})
	return model.RemoteEntry{ID: entryID, Name: name, Kind: model.KindFile, ParentID: model.Ptr("root"), Size: model.Ptr(int64(3))}, nil
}

func (f *fakeClient) Move(entryID string, sourceParentID string, destinationParentID string) error {
	f.moveCalls = append(f.moveCalls, [3]string{entryID, sourceParentID, destinationParentID})
	return nil
}

func (f *fakeClient) OpenDownload(entryID string, offset int64, length *int64, cid *string) (DownloadStream, error) {
	f.downloadCalls = append(f.downloadCalls, fakeDownload{entryID: entryID, offset: offset, length: length, cid: cid})
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	if f.stream != nil {
		return f.stream, nil
	}
	return &fakeStream{}, nil
}

type fakeStream struct {
	closed bool
}

func (s *fakeStream) Read(p []byte) (int, error) { return 0, io.EOF }
func (s *fakeStream) Close() error               { s.closed = true; return nil }
func (s *fakeStream) HTTPStatus() int            { return 200 }
func (s *fakeStream) ContentType() *string       { return model.Ptr("text/plain") }
func (s *fakeStream) ContentLength() *int64      { return model.Ptr(int64(11)) }
func (s *fakeStream) ContentRange() *string      { return nil }

func testBudget(t *testing.T) *budget.Budget {
	t.Helper()
	b, err := budget.New(budget.Config{
		MaxUploads:          1,
		MaxDownloads:        1,
		MaxConnections:      4,
		TransferWaitTimeout: 0.05,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newTestStorage(t *testing.T, client *fakeClient, mutate func(*StorageConfig)) *Storage {
	t.Helper()
	config := DefaultStorageConfig("root")
	config.CacheTTLSeconds = 60
	config.Writer = client
	config.Downloader = client
	if mutate != nil {
		mutate(&config)
	}
	storage, err := NewStorage(client, testBudget(t), config)
	if err != nil {
		t.Fatal(err)
	}
	return storage
}

func TestDefaultStorageConfigMirrorsPython(t *testing.T) {
	config := DefaultStorageConfig("0")
	if config.RootName != "WPS Enterprise Drive" || config.ListCount != 20 ||
		config.MaxListEntries != 10000 || config.CacheTTLSeconds != 2.0 ||
		config.MaxCachedFolders != 1024 || config.TransferWaitTimeout != 30.0 ||
		config.MaxCopyEntries != 10000 || config.MaxCopyDepth != 64 {
		t.Fatalf("defaults drifted: %+v", config)
	}
}

func TestNewStorageValidates(t *testing.T) {
	base := DefaultStorageConfig("root")
	cases := []struct {
		name    string
		mutate  func(*StorageConfig)
		message string
	}{
		{name: "root", mutate: func(c *StorageConfig) { c.RootID = "" }, message: "root_id is required"},
		{name: "list count", mutate: func(c *StorageConfig) { c.ListCount = -1 }, message: "list_count must be positive"},
		{name: "max list entries", mutate: func(c *StorageConfig) { c.MaxListEntries = -1 }, message: "max_list_entries must be positive"},
		{name: "count above max", mutate: func(c *StorageConfig) { c.MaxListEntries = 10 }, message: "list_count must not exceed max_list_entries"},
		{name: "ttl", mutate: func(c *StorageConfig) { c.CacheTTLSeconds = -1 }, message: "cache_ttl must not be negative"},
		{name: "folders", mutate: func(c *StorageConfig) { c.MaxCachedFolders = -1 }, message: "max_cached_folders must be positive"},
		{name: "wait", mutate: func(c *StorageConfig) { c.TransferWaitTimeout = -1 }, message: "transfer_wait_timeout must be positive"},
		{name: "copy entries", mutate: func(c *StorageConfig) { c.MaxCopyEntries = -1 }, message: "max_copy_entries must be positive"},
		{name: "copy depth", mutate: func(c *StorageConfig) { c.MaxCopyDepth = -1 }, message: "max_copy_depth must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := base
			tc.mutate(&config)
			_, err := NewStorage(newFakeClient(), testBudget(t), config)
			if err == nil || err.Error() != tc.message {
				t.Fatalf("NewStorage error = %v, want %q", err, tc.message)
			}
		})
	}
	_, err := NewStorage(nil, testBudget(t), base)
	if err == nil || err.Error() != "a wps lister is required" {
		t.Fatalf("nil lister error = %v", err)
	}
	_, err = NewStorage(newFakeClient(), nil, base)
	if err == nil || err.Error() != "a transfer budget is required" {
		t.Fatalf("nil budget error = %v", err)
	}
}

func TestResolvesNestedPathsUsingParentIDs(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	entry, err := storage.Metadata("/docs/readme.txt")
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID != "readme" {
		t.Fatalf("metadata id = %q", entry.ID)
	}
	children, err := storage.ListPath("/")
	if err != nil {
		t.Fatal(err)
	}
	if names := entryNames(children); fmt.Sprint(names) != "[docs top.txt]" {
		t.Fatalf("root listing = %v", names)
	}
	// The root listing is cached: a second resolve adds no upstream call.
	if _, err := storage.Metadata("/docs/readme.txt"); err != nil {
		t.Fatal(err)
	}
	if len(client.listCalls) != 2 {
		t.Fatalf("list calls = %v, want root+docs once", client.listCalls)
	}
}

func TestChildMatchingRules(t *testing.T) {
	entries := []model.RemoteEntry{
		{ID: "a1", Name: "a", Kind: model.KindFolder, ParentID: model.Ptr("p")},
		{ID: "a2", Name: "a", Kind: model.KindFile, ParentID: model.Ptr("p")},
		{ID: "u", Name: "a", Kind: model.KindUnknown},
		{ID: "moved", Name: "b", Kind: model.KindFile, ParentID: model.Ptr("elsewhere")},
	}
	if _, err := child("p", "missing", entries); err == nil || err.Error() != "entry not found: missing" {
		t.Fatalf("missing error = %v", err)
	}
	// Unknown kinds never match, so two real matches are ambiguous.
	if _, err := child("p", "a", entries); err == nil || err.Error() != "multiple entries have the name: a" {
		t.Fatalf("ambiguous error = %v", err)
	}
	unknownOnly := []model.RemoteEntry{{ID: "u", Name: "a", Kind: model.KindUnknown}}
	if _, err := child("p", "a", unknownOnly); err == nil || err.Error() != "entry not found: a" {
		t.Fatalf("unknown-kind error = %v", err)
	}
	if _, err := child("p", "b", entries); err == nil || err.Error() != "entry not found: b" {
		t.Fatalf("parent mismatch error = %v", err)
	}
}

func TestFileDrillDownIsNotFolder(t *testing.T) {
	storage := newTestStorage(t, newFakeClient(), nil)
	_, err := storage.Metadata("/top.txt/readme.txt")
	if err == nil || err.Error() != "not a folder: top.txt" {
		t.Fatalf("drill-down error = %v", err)
	}
}

func TestMetadataCacheHasAFolderCountBound(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, func(c *StorageConfig) { c.MaxCachedFolders = 1 })
	if _, err := storage.ListPath("/"); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ListPath("/docs"); err != nil {
		t.Fatal(err)
	}
	if len(client.listCalls) != 2 {
		t.Fatalf("list calls = %v", client.listCalls)
	}
	// After the two listings the single cache slot holds docs (root was
	// evicted on insert). The next resolution therefore re-fetches root
	// first — which in turn evicts docs — and then docs itself: four calls
	// total. A wrong eviction order would show only three.
	if _, err := storage.ListPath("/docs"); err != nil {
		t.Fatal(err)
	}
	if len(client.listCalls) != 4 {
		t.Fatalf("unexpected eviction behaviour: %v", client.listCalls)
	}
}

func TestUploadsNewPathAndRejectsCollision(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	result, err := storage.UploadPath(context.Background(), "/docs/new.txt", sizedReader("hello"), UploadOptions{Size: model.Ptr(int64(5))})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "new" {
		t.Fatalf("upload id = %q", result.ID)
	}
	call := client.uploadCalls[0]
	if call.parentID != "docs" || call.name != "new.txt" || call.request.Size == nil || *call.request.Size != 5 || string(call.body) != "hello" {
		t.Fatalf("upload call = %+v body %q", call, call.body)
	}
	_, err = storage.UploadPath(context.Background(), "/docs/readme.txt", sizedReader("overwrite"), UploadOptions{Size: model.Ptr(int64(9))})
	if err == nil || err.Error() != "overwrite is not enabled for: /docs/readme.txt" {
		t.Fatalf("collision error = %v", err)
	}
	kind, ok := model.AsStorageError(err)
	if !ok || kind.Kind != model.KindAlreadyExists {
		t.Fatalf("collision error kind = %v", err)
	}
}

func TestOverwritesExistingFileOnlyWhenEnabled(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	if _, err := storage.UploadPath(context.Background(), "/top.txt", sizedReader("new"), UploadOptions{Size: model.Ptr(int64(3)), Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if !client.uploadCalls[0].request.Overwrite {
		t.Fatal("overwrite flag not passed through")
	}
	// A folder cannot be overwritten even when enabled.
	if _, err := storage.UploadPath(context.Background(), "/docs", sizedReader("new"), UploadOptions{Size: model.Ptr(int64(3)), Overwrite: true}); err == nil || err.Error() != "overwrite is not enabled for: /docs" {
		t.Fatalf("folder overwrite error = %v", err)
	}
}

func TestUploadSlotReleasedOnAllReturnPaths(t *testing.T) {
	client := newFakeClient()
	client.uploadErr = errors.New("upstream upload failed")
	storage := newTestStorage(t, client, nil)
	ctx := context.Background()
	if _, err := storage.UploadPath(ctx, "/docs/x.txt", sizedReader("x"), UploadOptions{}); err == nil {
		t.Fatal("upload error swallowed")
	}
	if active := storage.budget.Stats().UploadsActive; active != 0 {
		t.Fatalf("upload slot leaked: %d active", active)
	}
	// The slot is free again: the next upload reaches the writer.
	client.uploadErr = nil
	if _, err := storage.UploadPath(ctx, "/docs/x.txt", sizedReader("x"), UploadOptions{}); err != nil {
		t.Fatalf("second upload failed: %v", err)
	}
}

func TestRenamesPathAndRejectsCollision(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	result, err := storage.RenamePath("/top.txt", "renamed.txt")
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "renamed.txt" {
		t.Fatalf("rename result = %q", result.Name)
	}
	if fmt.Sprint(client.renameCalls) != "[[top renamed.txt]]" {
		t.Fatalf("rename calls = %v", client.renameCalls)
	}
	_, err = storage.RenamePath("/top.txt", "docs")
	if err == nil || err.Error() != "entry already exists: docs" {
		t.Fatalf("collision error = %v", err)
	}
	// Same-name rename is a no-op that never calls the writer.
	calls := len(client.renameCalls)
	same, err := storage.RenamePath("/top.txt", "top.txt")
	if err != nil || same.Name != "top.txt" || len(client.renameCalls) != calls {
		t.Fatalf("same-name rename = %v, %v, calls %d", same, err, len(client.renameCalls))
	}
}

func TestRenamesByID(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	result, err := storage.Rename("top", "renamed.txt")
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "renamed.txt" || fmt.Sprint(client.renameCalls) != "[[top renamed.txt]]" {
		t.Fatalf("rename = %v, calls %v", result, client.renameCalls)
	}
	_, err = storage.Rename("top", "a/b")
	if err == nil || err.Error() != "name must be one remote path component" {
		t.Fatalf("invalid name error = %v", err)
	}
}

func TestDeletesPathAndRejectsRoot(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	if err := storage.DeletePath("/top.txt"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(client.deleteCalls) != "[top]" {
		t.Fatalf("delete calls = %v", client.deleteCalls)
	}
	err := storage.DeletePath("/")
	if err == nil || err.Error() != "the root cannot be deleted" {
		t.Fatalf("root delete error = %v", err)
	}
	// Successful deletion invalidated the cache: listing hits upstream again.
	calls := len(client.listCalls)
	if _, err := storage.ListPath("/"); err != nil {
		t.Fatal(err)
	}
	if len(client.listCalls) == calls {
		t.Fatal("cache survived a successful delete")
	}
}

func TestMovesPathToDestinationParent(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	result, err := storage.MoveToParentPath("/top.txt", "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if result.ParentID == nil || *result.ParentID != "docs" {
		t.Fatalf("moved parent = %v", result.ParentID)
	}
	if fmt.Sprint(client.moveCalls) != "[[top root docs]]" {
		t.Fatalf("move calls = %v", client.moveCalls)
	}
	// The rebuilt entry keeps the transfer fields but no raw payload.
	if result.Raw != nil {
		t.Fatal("moved entry carries a raw payload")
	}
}

func TestMoveIntoItselfRejected(t *testing.T) {
	storage := newTestStorage(t, newFakeClient(), nil)
	_, err := storage.MoveToParentPath("/docs", "/docs/sub")
	if err == nil || err.Error() != "an entry cannot be moved into itself" {
		t.Fatalf("move-into-self error = %v", err)
	}
	_, err = storage.MoveToParentPath("/docs", "/docs")
	if err == nil || err.Error() != "an entry cannot be moved into itself" {
		t.Fatalf("move-onto-self error = %v", err)
	}
}

func TestMovePathDispatchesLikePython(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	// Same parent, different name → rename.
	if _, err := storage.MovePath("/top.txt", "/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(client.renameCalls) != "[[top renamed.txt]]" || len(client.moveCalls) != 0 {
		t.Fatalf("rename dispatch calls = %v %v", client.renameCalls, client.moveCalls)
	}
	// Cross-folder rename is unsupported.
	_, err := storage.MovePath("/top.txt", "/docs/other.txt")
	if err == nil || err.Error() != "cross-folder move with rename is not supported" {
		t.Fatalf("cross-folder rename error = %v", err)
	}
	kind, _ := model.AsStorageError(err)
	if kind.Kind != model.KindUnsupportedOperation {
		t.Fatalf("cross-folder rename kind = %v", kind.Kind)
	}
	// Same name into another folder → move.
	client.renameCalls = nil
	if _, err := storage.MovePath("/top.txt", "/docs/top.txt"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(client.moveCalls) != "[[top root docs]]" {
		t.Fatalf("move dispatch calls = %v", client.moveCalls)
	}
	// Same-parent same-name move short-circuits without a writer call.
	client.moveCalls = nil
	moved, err := storage.MovePath("/top.txt", "/top.txt")
	if err != nil || moved.ID != "top" || len(client.moveCalls) != 0 {
		t.Fatalf("same-place move = %v, %v, %v", moved, err, client.moveCalls)
	}
}

func TestDownloadUsesFileLinkIDAsCID(t *testing.T) {
	client := newFakeClient()
	client.children["root"] = []model.RemoteEntry{
		{ID: "top", Name: "top.txt", Kind: model.KindFile, ParentID: model.Ptr("root"), Size: model.Ptr(int64(3)), LinkID: model.Ptr("file-link-cid")},
	}
	storage := newTestStorage(t, client, nil)
	stream, err := storage.OpenPath(context.Background(), "/top.txt", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	call := client.downloadCalls[0]
	if call.entryID != "top" || call.offset != 0 || call.length != nil || call.cid == nil || *call.cid != "file-link-cid" {
		t.Fatalf("download call = %+v", call)
	}
}

func TestDownloadSlotBoundToOpenAndClose(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	stream, err := storage.OpenPath(context.Background(), "/top.txt", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if active := storage.budget.Stats().DownloadsActive; active != 1 {
		t.Fatalf("active downloads = %d, want 1 while stream is open", active)
	}
	// Capacity is one: a second open must time out into the busy error.
	_, err = storage.OpenPath(context.Background(), "/top.txt", 0, nil)
	if err == nil || err.Error() != "too many downloads are active" {
		t.Fatalf("second open error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if active := storage.budget.Stats().DownloadsActive; active != 0 {
		t.Fatalf("active after close = %d", active)
	}
	// Close is idempotent: the slot is not released twice.
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if active := storage.budget.Stats().DownloadsActive; active != 0 {
		t.Fatalf("active after double close = %d", active)
	}
	stream2, err := storage.OpenPath(context.Background(), "/top.txt", 0, nil)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	stream2.Close()
}

func TestDownloadSlotReleasedOnOpenError(t *testing.T) {
	client := newFakeClient()
	client.downloadErr = errors.New("upstream download failed")
	storage := newTestStorage(t, client, nil)
	if _, err := storage.OpenPath(context.Background(), "/top.txt", 0, nil); err == nil {
		t.Fatal("open error swallowed")
	}
	if active := storage.budget.Stats().DownloadsActive; active != 0 {
		t.Fatalf("download slot leaked: %d active", active)
	}
}

func TestDownloadRejectsFolders(t *testing.T) {
	storage := newTestStorage(t, newFakeClient(), nil)
	_, err := storage.OpenPath(context.Background(), "/docs", 0, nil)
	if err == nil || err.Error() != "not a downloadable file: /docs" {
		t.Fatalf("folder download error = %v", err)
	}
}

func TestWritesRefusedWithoutWriter(t *testing.T) {
	storage := newTestStorage(t, newFakeClient(), func(c *StorageConfig) { c.Writer = nil })
	ctx := context.Background()
	if _, err := storage.UploadPath(ctx, "/docs/new.txt", sizedReader("x"), UploadOptions{}); err == nil || err.Error() != "write operations are not wired in this stage" {
		t.Fatalf("upload error = %v", err)
	}
	if _, err := storage.CreateFolder(nil, "x"); err == nil || err.Error() != "write operations are not wired in this stage" {
		t.Fatalf("create folder error = %v", err)
	}
	if _, err := storage.CreateFolderPath("/x"); err == nil || err.Error() != "write operations are not wired in this stage" {
		t.Fatalf("create folder path error = %v", err)
	}
	if err := storage.Delete("x"); err == nil || err.Error() != "write operations are not wired in this stage" {
		t.Fatalf("delete error = %v", err)
	}
	if err := storage.DeletePath("/top.txt"); err == nil || err.Error() != "write operations are not wired in this stage" {
		t.Fatalf("delete path error = %v", err)
	}
	if _, err := storage.Rename("top", "x"); err == nil || err.Error() != "write operations are not wired in this stage" {
		t.Fatalf("rename error = %v", err)
	}
	if _, err := storage.RenamePath("/top.txt", "x"); err == nil || err.Error() != "write operations are not wired in this stage" {
		t.Fatalf("rename path error = %v", err)
	}
	if _, err := storage.MovePath("/top.txt", "/docs/top.txt"); err == nil || err.Error() != "write operations are not wired in this stage" {
		t.Fatalf("move error = %v", err)
	}
}

func TestDownloadsRefusedWithoutDownloader(t *testing.T) {
	storage := newTestStorage(t, newFakeClient(), func(c *StorageConfig) { c.Downloader = nil })
	if _, err := storage.OpenPath(context.Background(), "/top.txt", 0, nil); err == nil || err.Error() != "download operations are not wired in this stage" {
		t.Fatalf("open path error = %v", err)
	}
	if _, err := storage.OpenDownload("top", 0); err == nil || err.Error() != "download operations are not wired in this stage" {
		t.Fatalf("open download error = %v", err)
	}
	if active := storage.budget.Stats().DownloadsActive; active != 0 {
		t.Fatalf("download slot leaked: %d", active)
	}
}

func TestCreatesFolderPathAndRejectsCollision(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	result, err := storage.CreateFolderPath("/new-folder")
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != model.KindFolder {
		t.Fatalf("created kind = %v", result.Kind)
	}
	if fmt.Sprint(client.folderCalls) != "[[root new-folder]]" {
		t.Fatalf("folder calls = %v", client.folderCalls)
	}
	_, err = storage.CreateFolderPath("/docs")
	if err == nil || err.Error() != "entry already exists: /docs" {
		t.Fatalf("collision error = %v", err)
	}
	// CreateFolder by ID: nil parent means the virtual root.
	if _, err := storage.CreateFolder(nil, "other"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(client.folderCalls[len(client.folderCalls)-1]) != "[root other]" {
		t.Fatalf("by-id folder calls = %v", client.folderCalls)
	}
	_, err = storage.CreateFolder(nil, "a/b")
	if err == nil || err.Error() != "folder name must be one remote path component" {
		t.Fatalf("invalid folder name error = %v", err)
	}
}

func TestListByIDUsesRootWhenNil(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	children, err := storage.ListByID(nil)
	if err != nil {
		t.Fatal(err)
	}
	if names := entryNames(children); fmt.Sprint(names) != "[docs top.txt]" {
		t.Fatalf("by-root listing = %v", names)
	}
	docsChildren, err := storage.ListByID(model.Ptr("docs"))
	if err != nil {
		t.Fatal(err)
	}
	if names := entryNames(docsChildren); fmt.Sprint(names) != "[readme.txt]" {
		t.Fatalf("by-id listing = %v", names)
	}
}

func TestWaitTimeoutSurfacesAsBusy(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	held, err := storage.budget.AcquireUpload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err = storage.UploadPath(ctx, "/docs/new.txt", sizedReader("x"), UploadOptions{})
	if err == nil || err.Error() != "too many uploads are active" {
		t.Fatalf("upload wait error = %v", err)
	}
}

func entryNames(entries []model.RemoteEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name
	}
	return names
}

type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func sizedReader(s string) io.Reader {
	return &byteReader{data: []byte(s)}
}
