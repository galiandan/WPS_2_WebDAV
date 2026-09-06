package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// copyWriter wraps the shared fake client with the native Copy surface;
// the plain fakeClient stays Copy-less, so storages built with it relay.
type copyWriter struct {
	*fakeClient
	calls [][2]string
}

func (w *copyWriter) Copy(fileID string, targetParentID string) (string, error) {
	w.calls = append(w.calls, [2]string{fileID, targetParentID})
	return "copied-9", nil
}

// byteStream serves fixed bytes with an idempotent close.
type byteStream struct {
	once   sync.Once
	reader io.Reader
	closed bool
}

func newByteStream(content []byte) *byteStream {
	return &byteStream{reader: bytes.NewReader(content)}
}

func (s *byteStream) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *byteStream) Close() error {
	s.once.Do(func() { s.closed = true })
	return nil
}
func (s *byteStream) HTTPStatus() int       { return 200 }
func (s *byteStream) ContentType() *string  { return nil }
func (s *byteStream) ContentLength() *int64 { return nil }
func (s *byteStream) ContentRange() *string { return nil }

// blockingStream blocks reads until closed, standing in for an upstream
// connection that only unblocks when the transfer is torn down.
type blockingStream struct {
	once    sync.Once
	closed  chan struct{}
	closers []func()
}

func newBlockingStream() *blockingStream {
	return &blockingStream{closed: make(chan struct{})}
}

func (s *blockingStream) Read(p []byte) (int, error) {
	<-s.closed
	return 0, errors.New("stream closed by cancellation")
}

func (s *blockingStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *blockingStream) HTTPStatus() int       { return 200 }
func (s *blockingStream) ContentType() *string  { return nil }
func (s *blockingStream) ContentLength() *int64 { return nil }
func (s *blockingStream) ContentRange() *string { return nil }

func TestCopyPathNativeSingleFile(t *testing.T) {
	client := newFakeClient()
	writer := &copyWriter{fakeClient: client}
	storage := newTestStorage(t, client, func(c *StorageConfig) { c.Writer = writer })

	entry, err := storage.CopyPath(context.Background(), "/top.txt", "/docs/top.txt", CopyOptions{Depth: "infinity", Overwrite: true})
	if err != nil {
		t.Fatalf("CopyPath failed: %v", err)
	}
	if entry.ID != "copied-9" || entry.Kind != model.KindFile || entry.Name != "top.txt" {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.ParentID == nil || *entry.ParentID != "docs" {
		t.Fatalf("parent = %v", entry.ParentID)
	}
	if entry.Size == nil || *entry.Size != 3 {
		t.Fatalf("size = %v, want the source size 3", entry.Size)
	}
	if got := fmt.Sprint(writer.calls); got != "[[top docs]]" {
		t.Fatalf("copy calls = %v", got)
	}
	if len(client.uploadCalls) != 0 || len(client.folderCalls) != 0 {
		t.Fatalf("the native copy must not upload or create folders: %+v %+v", client.uploadCalls, client.folderCalls)
	}
	if len(client.downloadCalls) != 0 {
		t.Fatalf("the native copy must not download bytes: %+v", client.downloadCalls)
	}
}

func TestCopyPathRelaysRenamedFilesWithGuessedType(t *testing.T) {
	client := newFakeClient()
	client.children["root"][1].LinkID = model.Ptr("link-1")
	client.stream = newByteStream([]byte("bench-bytes"))
	storage := newTestStorage(t, client, nil)

	entry, err := storage.CopyPath(context.Background(), "/top.txt", "/docs/renamed.txt", CopyOptions{Depth: "infinity"})
	if err != nil {
		t.Fatalf("CopyPath failed: %v", err)
	}
	if entry.Name != "renamed.txt" || entry.Kind != model.KindFile {
		t.Fatalf("entry = %+v", entry)
	}
	if len(client.uploadCalls) != 1 {
		t.Fatalf("upload calls = %d", len(client.uploadCalls))
	}
	upload := client.uploadCalls[0]
	if upload.parentID != "docs" || upload.name != "renamed.txt" {
		t.Fatalf("upload target = %s/%s", upload.parentID, upload.name)
	}
	if string(upload.body) != "bench-bytes" {
		t.Fatalf("upload body = %q", upload.body)
	}
	if upload.request.ContentType != "text/plain" {
		t.Fatalf("content type = %q, want the guessed text/plain", upload.request.ContentType)
	}
	if upload.request.Size == nil || *upload.request.Size != 3 {
		t.Fatalf("declared size = %v, want the source size 3", upload.request.Size)
	}
	if upload.request.Overwrite {
		t.Fatal("the relay never overwrites")
	}
	if len(client.downloadCalls) != 1 || client.downloadCalls[0].entryID != "top" || client.downloadCalls[0].cid == nil || *client.downloadCalls[0].cid != "link-1" {
		t.Fatalf("download calls = %+v", client.downloadCalls)
	}
}

func TestCopyPathRefusesExistingDestinationWithoutDeleting(t *testing.T) {
	client := newFakeClient()
	writer := &copyWriter{fakeClient: client}
	storage := newTestStorage(t, client, func(c *StorageConfig) { c.Writer = writer })

	// Overwrite F (the REST-like default here) reports the conflict.
	_, err := storage.CopyPath(context.Background(), "/top.txt", "/docs/readme.txt", CopyOptions{Depth: "infinity"})
	if err == nil || err.Error() != "entry already exists: /docs/readme.txt" {
		t.Fatalf("existing without overwrite error = %v", err)
	}
	// Overwrite T refuses because the relay is not atomic.
	_, err = storage.CopyPath(context.Background(), "/top.txt", "/docs/readme.txt", CopyOptions{Depth: "infinity", Overwrite: true})
	storageErr, ok := model.AsStorageError(err)
	if !ok || storageErr.Kind != model.KindUnsupportedOperation ||
		storageErr.Message != "COPY overwrite is disabled because the relay is not atomic" {
		t.Fatalf("existing with overwrite error = %v", err)
	}
	if len(writer.calls) != 0 || len(client.uploadCalls) != 0 || len(client.folderCalls) != 0 || len(client.deleteCalls) != 0 {
		t.Fatal("an existing destination must never be touched, let alone deleted first")
	}
}

func TestCopyPathValidation(t *testing.T) {
	client := newFakeClient()
	storage := newTestStorage(t, client, nil)
	cases := []struct {
		source      string
		destination string
		depth       string
		message     string
	}{
		{"/top.txt", "/docs/x.txt", "2", "COPY Depth must be 0, 1 or infinity"},
		{"/", "/docs/", "infinity", "the root cannot be copied"},
		{"/top.txt", "/", "infinity", "the root cannot be copied"},
		{"/top.txt", "/top.txt", "infinity", "an entry cannot be copied onto itself"},
		{"/docs", "/docs/inner/", "infinity", "a folder cannot be copied into itself"},
		{"/top.txt", "/docs/readme.txt/inner", "infinity", "the COPY destination parent is not a folder"},
	}
	for _, tc := range cases {
		_, err := storage.CopyPath(context.Background(), tc.source, tc.destination, CopyOptions{Depth: tc.depth})
		if err == nil || err.Error() != tc.message {
			t.Fatalf("(%s -> %s, depth %s) error = %v, want %q", tc.source, tc.destination, tc.depth, err, tc.message)
		}
		if len(client.uploadCalls) != 0 && tc.destination != "/docs/readme.txt/inner" {
			t.Fatalf("case %s -> %s uploaded despite the refusal", tc.source, tc.destination)
		}
	}
}

func copyFolderFixture() *fakeClient {
	client := newFakeClient()
	client.children["docs"] = []model.RemoteEntry{
		{ID: "sub", Name: "sub", Kind: model.KindFolder, ParentID: model.Ptr("docs"), Size: model.Ptr(int64(0))},
		{ID: "readme", Name: "readme.txt", Kind: model.KindFile, ParentID: model.Ptr("docs"), Size: model.Ptr(int64(4))},
	}
	client.children["sub"] = []model.RemoteEntry{
		{ID: "deep", Name: "deep.txt", Kind: model.KindFile, ParentID: model.Ptr("sub"), Size: model.Ptr(int64(2))},
	}
	client.stream = newByteStream([]byte("bytes"))
	return client
}

func TestCopyPathFolderDepthSemantics(t *testing.T) {
	// Depth 0 creates only the destination root.
	client := copyFolderFixture()
	storage := newTestStorage(t, client, nil)
	if _, err := storage.CopyPath(context.Background(), "/docs", "/docs-copy", CopyOptions{Depth: "0"}); err != nil {
		t.Fatal(err)
	}
	if len(client.folderCalls) != 1 || client.folderCalls[0] != [2]string{"root", "docs-copy"} {
		t.Fatalf("depth 0 folders = %v", client.folderCalls)
	}
	if len(client.uploadCalls) != 0 {
		t.Fatalf("depth 0 must not upload files: %+v", client.uploadCalls)
	}

	// Depth 1 copies direct children but never descends.
	client = copyFolderFixture()
	storage = newTestStorage(t, client, nil)
	if _, err := storage.CopyPath(context.Background(), "/docs", "/docs-copy", CopyOptions{Depth: "1"}); err != nil {
		t.Fatal(err)
	}
	if len(client.folderCalls) != 2 || client.folderCalls[1] != [2]string{"folder-new", "sub"} {
		t.Fatalf("depth 1 folders = %v", client.folderCalls)
	}
	if len(client.uploadCalls) != 1 || client.uploadCalls[0].name != "readme.txt" {
		t.Fatalf("depth 1 uploads = %+v", client.uploadCalls)
	}

	// Infinity copies the whole tree.
	client = copyFolderFixture()
	storage = newTestStorage(t, client, nil)
	entry, err := storage.CopyPath(context.Background(), "/docs", "/docs-copy", CopyOptions{Depth: "infinity"})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "docs-copy" {
		t.Fatalf("root entry = %+v", entry)
	}
	if len(client.folderCalls) != 2 {
		t.Fatalf("infinity folders = %v", client.folderCalls)
	}
	if len(client.uploadCalls) != 2 {
		t.Fatalf("infinity uploads = %+v", client.uploadCalls)
	}
	if client.uploadCalls[0].parentID != "folder-new" || client.uploadCalls[1].parentID != "folder-new" {
		t.Fatalf("nested upload parents = %+v", client.uploadCalls)
	}
}

func TestCopyPathEntryAndDepthLimits(t *testing.T) {
	// The entry limit counts every visited node.
	client := copyFolderFixture()
	storage := newTestStorage(t, client, func(c *StorageConfig) { c.MaxCopyEntries = 2 })
	_, err := storage.CopyPath(context.Background(), "/docs", "/docs-copy", CopyOptions{Depth: "infinity"})
	if err == nil || err.Error() != "COPY exceeds the configured entry limit" {
		t.Fatalf("entry limit error = %v", err)
	}

	// The depth limit cuts the recursion: the source folder is level 0,
	// sub is level 1, so a max depth of 1 refuses below it.
	client = copyFolderFixture()
	storage = newTestStorage(t, client, func(c *StorageConfig) { c.MaxCopyDepth = 1 })
	_, err = storage.CopyPath(context.Background(), "/docs", "/docs-copy", CopyOptions{Depth: "infinity"})
	if err == nil || err.Error() != "COPY exceeds the configured depth limit" {
		t.Fatalf("depth limit error = %v", err)
	}
}

func TestCopyPathCleansCreatedFoldersOnFailure(t *testing.T) {
	client := copyFolderFixture()
	storage := newTestStorage(t, client, func(c *StorageConfig) { c.MaxCopyEntries = 2 })

	_, err := storage.CopyPath(context.Background(), "/docs", "/docs-copy", CopyOptions{Depth: "infinity"})
	if err == nil {
		t.Fatal("expected the entry limit failure")
	}
	// The sub-folder was created before the limit hit; both created folders
	// are removed best-effort (the cascade runs at each level).
	if len(client.deleteCalls) != 2 {
		t.Fatalf("cleanup deletes = %v", client.deleteCalls)
	}
	// A failing cleanup is swallowed and must not mask the copy error.
	client = copyFolderFixture()
	client.deleteErr = errors.New("delete refused")
	storage = newTestStorage(t, client, func(c *StorageConfig) { c.MaxCopyEntries = 2 })
	_, err = storage.CopyPath(context.Background(), "/docs", "/docs-copy", CopyOptions{Depth: "infinity"})
	if err == nil || err.Error() != "COPY exceeds the configured entry limit" {
		t.Fatalf("cleanup failure must not mask the copy error: %v", err)
	}
}

func TestCopyPathCancellationClosesBothSides(t *testing.T) {
	client := newFakeClient()
	stream := newBlockingStream()
	client.stream = stream
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	client.uploadStarted = func() { close(started) }
	storage := newTestStorage(t, client, nil)

	done := make(chan error, 1)
	go func() {
		_, err := storage.CopyPath(ctx, "/top.txt", "/docs/top-copy.txt", CopyOptions{Depth: "infinity"})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the cancelled copy must fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled copy did not return")
	}
	select {
	case <-stream.closed:
	default:
		t.Fatal("cancellation must close the source stream")
	}
}

func TestMultiSpaceCopyPath(t *testing.T) {
	multi, spy := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{
			{Name: "alpha", GroupID: "gA", RootID: "root"},
			{Name: "beta", GroupID: "gB", RootID: "root"},
		}
	})
	if _, err := multi.CopyPath(context.Background(), "/alpha/top.txt", "/beta/other.txt", CopyOptions{Depth: "infinity"}); err == nil ||
		err.Error() != "cross-space copy is not supported" {
		t.Fatalf("cross-space copy error = %v", err)
	}
	entry, err := multi.CopyPath(context.Background(), "/alpha/top.txt", "/alpha/other.txt", CopyOptions{Depth: "infinity"})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "other.txt" || entry.Kind != model.KindFile {
		t.Fatalf("entry = %+v", entry)
	}
	if len(spy.clients["gA"].uploadCalls) != 1 {
		t.Fatalf("alpha uploads = %+v", spy.clients["gA"].uploadCalls)
	}
	if len(spy.clients["gB"].uploadCalls) != 0 {
		t.Fatalf("beta uploads = %+v", spy.clients["gB"].uploadCalls)
	}
}
