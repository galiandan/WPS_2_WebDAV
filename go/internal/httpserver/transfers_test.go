package httpserver

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/remotefetch"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/transfers"
)

type transferUploadCall struct {
	path    string
	body    []byte
	options storage.UploadOptions
}
type transferStorageFake struct {
	mu            sync.Mutex
	entries       map[string]model.RemoteEntry
	children      map[string][]model.RemoteEntry
	data          map[string]string
	uploads       []transferUploadCall
	uploadErr     error
	onUpload      func()
	onOpen        func(string)
	afterMetadata func(string)
}

func (s *transferStorageFake) Metadata(path string) (model.RemoteEntry, error) {
	s.mu.Lock()
	entry, ok := s.entries[path]
	hook := s.afterMetadata
	s.mu.Unlock()
	if !ok {
		return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "entry not found")
	}
	if hook != nil {
		hook(path)
	}
	return entry, nil
}
func (s *transferStorageFake) ListPath(path string) ([]model.RemoteEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.RemoteEntry(nil), s.children[path]...), nil
}
func (s *transferStorageFake) ListChildren(path string, _ model.RemoteEntry) ([]model.RemoteEntry, error) {
	return s.ListPath(path)
}
func (s *transferStorageFake) OpenPath(_ context.Context, path string, _ int64, _ *int64) (DownloadStream, error) {
	s.mu.Lock()
	hook := s.onOpen
	body := s.data[path]
	s.mu.Unlock()
	if hook != nil {
		hook(path)
	}
	return newFakeStream(body, model.Ptr(int64(len(body)))), nil
}
func (s *transferStorageFake) UploadPath(_ context.Context, path string, source io.Reader, options storage.UploadOptions) (model.RemoteEntry, error) {
	s.mu.Lock()
	hook := s.onUpload
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	body, err := io.ReadAll(source)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads = append(s.uploads, transferUploadCall{path: path, body: body, options: options})
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if s.uploadErr != nil {
		return model.RemoteEntry{}, s.uploadErr
	}
	entry := archiveFile("uploaded-id", filepath.Base(path), string(body))
	s.entries[path] = entry
	return entry, nil
}
func (s *transferStorageFake) replaceID(path, id string) {
	s.mu.Lock()
	entry := s.entries[path]
	entry.ID = id
	s.entries[path] = entry
	s.mu.Unlock()
}
func (s *transferStorageFake) uploadSnapshot() []transferUploadCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]transferUploadCall(nil), s.uploads...)
}

type transferFetcherFake struct {
	mu      sync.Mutex
	body    string
	unknown bool
	called  int
	before  func(context.Context, io.Writer) error
	after   func()
}

func (f *transferFetcherFake) Fetch(ctx context.Context, _ string, writer io.Writer, limit int64, progress func(int64)) (remotefetch.Result, error) {
	f.mu.Lock()
	f.called++
	body, unknown, before, after := f.body, f.unknown, f.before, f.after
	f.mu.Unlock()
	if before != nil {
		if err := before(ctx, writer); err != nil {
			return remotefetch.Result{}, err
		}
	}
	if int64(len(body)) > limit {
		return remotefetch.Result{}, remotefetch.ErrTooLarge
	}
	if size, ok := writer.(remotefetch.ExpectedSizer); ok {
		expected := int64(len(body))
		if unknown {
			expected = -1
		}
		if err := size.SetExpectedSize(expected); err != nil {
			return remotefetch.Result{}, err
		}
	}
	n, err := io.WriteString(writer, body)
	if progress != nil {
		progress(int64(n))
	}
	if after != nil {
		after()
	}
	return remotefetch.Result{Bytes: int64(n), ContentType: "application/octet-stream"}, err
}
func (f *transferFetcherFake) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.called }

type transferFixture struct {
	controller *TransferController
	dispatcher *RESTDispatcher
	storage    *transferStorageFake
	fetcher    *transferFetcherFake
	budget     *budget.Budget
	config     TransferConfig
	mu         sync.Mutex
	actors     map[string]auth.Principal
	actor      auth.Principal
	identity   string
}

func newTransferFixture(t *testing.T) *transferFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := archiveFile("source-id", "literal%25 + 文.txt", "archive-content")
	f := &transferFixture{identity: strings.Repeat("c", 64), actor: auth.Principal{ID: strings.Repeat("a", 32), Username: "alice", Role: "member", PolicyVersion: 1, RootPath: "/", RootID: "root-id", Permissions: auth.Permissions{Read: true, Upload: true}}, storage: &transferStorageFake{entries: map[string]model.RemoteEntry{"/": archiveFolder("root-id", "root"), "/target": archiveFolder("parent-id", "target"), "/source": archiveFolder("folder-id", "source"), "/source/literal%25 + 文.txt": file}, children: map[string][]model.RemoteEntry{"/source": {file}}, data: map[string]string{"/source/literal%25 + 文.txt": "archive-content"}}, fetcher: &transferFetcherFake{body: "downloaded-exact-bytes"}}
	f.actors = map[string]auth.Principal{f.actor.ID: f.actor}
	cfg := budget.DefaultConfig()
	cfg.UploadSpoolDir = dir
	cfg.UploadSpoolMemory = 0
	cfg.UploadMinFreeBytes = 0
	cfg.MaxDownloads = 1
	cfg.TransferWaitTimeout = .1
	var err error
	f.budget, err = budget.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.dispatcher = newReadDispatcherDownloads(t, f.storage, nil, f.storage)
	f.dispatcher.uploads = f.storage
	f.dispatcher.maxUploadBytes = 1 << 20
	f.config = TransferConfig{File: filepath.Join(dir, "transfers.json"), Directory: filepath.Join(dir, "artifacts"), Budget: f.budget, Fetcher: f.fetcher,
		ResolveOwner: func(id string, version uint64) (auth.Principal, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			p, ok := f.actors[id]
			if !ok || p.PolicyVersion != version {
				return auth.Principal{}, transfers.ErrDenied
			}
			return p, nil
		},
		DispatcherFor: func(auth.Principal) (*RESTDispatcher, error) { return f.dispatcher, nil }, Identity: func(auth.Principal, []string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.identity, nil
		}}
	f.controller, err = NewTransferController(f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.controller.Close() })
	return f
}
func (f *transferFixture) request(t *testing.T, actor auth.Principal, method, suffix, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := writeRequest(method, "/api/v1/"+suffix, headers, body).WithContext(auth.WithPrincipal(context.Background(), actor))
	w := httptest.NewRecorder()
	if err := f.controller.Serve(w, request, RESTRoute{Suffix: suffix}); err != nil {
		mapError(w, request, err, true)
	}
	return w
}
func (f *transferFixture) submit(t *testing.T, body string) transfers.Task {
	t.Helper()
	response := f.request(t, f.actor, "POST", "transfers", body, nil)
	if response.Code != 202 {
		t.Fatalf("submit=%d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Task transfers.Task `json:"task"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Task
}
func (f *transferFixture) wait(t *testing.T, id string) transfers.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := f.controller.manager.GetOwned(id, f.actor.ID, f.actor.PolicyVersion)
		if err != nil {
			t.Fatal(err)
		}
		if task.State != "queued" && task.State != "running" {
			return task
		}
		time.Sleep(time.Millisecond)
	}
	task, _ := f.controller.manager.GetOwned(id, f.actor.ID, f.actor.PolicyVersion)
	t.Fatalf("transfer did not finish: %+v", task)
	return transfers.Task{}
}
func (f *transferFixture) assertReleased(t *testing.T, id string) {
	t.Helper()
	if stats := f.budget.Stats(); stats.DownloadsActive != 0 || stats.SpoolReservedBytes != 0 {
		t.Fatalf("resources leaked: %+v", stats)
	}
	if _, err := os.Stat(f.controller.filename(id, ".part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary data retained: %v", err)
	}
}

const transferFetchRequest = `{"kind":"fetch","url":"https://source.example/file?signature=private-source-token","destination":"/target/new.bin"}`

func TestTransferFetchUploadsExactNewFileWithBoundParent(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(map[bool]string{false: "member", true: "admin"}[admin], func(t *testing.T) {
			f := newTransferFixture(t)
			if admin {
				f.actor.ID = auth.InstallationID
				f.actor.Role = "admin"
				f.actor.PolicyVersion = 1<<60 + 17
				f.mu.Lock()
				f.actors[f.actor.ID] = f.actor
				f.mu.Unlock()
			}
			f.storage.onUpload = func() {
				data, err := os.ReadFile(f.config.File)
				if err != nil {
					t.Error(err)
				}
				if !bytes.Contains(data, []byte(`"remote_write":true`)) {
					t.Error("upload started without durable remote-write barrier")
				}
			}
			created := f.submit(t, transferFetchRequest)
			final := f.wait(t, created.ID)
			calls := f.storage.uploadSnapshot()
			if final.State != "completed" || len(calls) != 1 || string(calls[0].body) != f.fetcher.body || calls[0].path != "/target/new.bin" || calls[0].options.Overwrite || calls[0].options.ExpectedParentID != "parent-id" || calls[0].options.Size == nil || *calls[0].options.Size != int64(len(f.fetcher.body)) {
				t.Fatalf("task=%+v uploads=%+v", final, calls)
			}
			if final.CanRetry || final.CanCancel {
				t.Fatal("completed upload can run twice", final)
			}
			list := f.request(t, f.actor, "GET", "transfers", "", nil)
			if list.Code != 200 || strings.Contains(list.Body.String(), "private-source-token") || strings.Contains(list.Body.String(), "source.example") || strings.Contains(list.Body.String(), "parent-id") {
				t.Fatalf("source leaked: %d %s", list.Code, list.Body.String())
			}
			f.assertReleased(t, created.ID)
		})
	}
}

func TestTransferFetchRejectsPrivateURLsReadOnlyAndExistingTargets(t *testing.T) {
	f := newTransferFixture(t)
	for _, source := range []string{"http://127.0.0.1/private", "http://169.254.169.254/metadata", "http://[::ffff:10.0.0.1]/secret", "file:///etc/passwd", "https://user:secret@public.example/file"} {
		body, _ := json.Marshal(map[string]string{"kind": "fetch", "url": source, "destination": "/target/new.bin"})
		if response := f.request(t, f.actor, "POST", "transfers", string(body), nil); response.Code != 400 {
			t.Fatalf("private URL accepted: %d %s", response.Code, response.Body.String())
		}
	}
	f.storage.mu.Lock()
	f.storage.entries["/target/new.bin"] = archiveFile("existing-id", "new.bin", "old")
	f.storage.mu.Unlock()
	if response := f.request(t, f.actor, "POST", "transfers", transferFetchRequest, nil); response.Code != 409 {
		t.Fatalf("overwrite accepted=%d %s", response.Code, response.Body.String())
	}
	f.actor.Permissions.Upload = false
	f.mu.Lock()
	f.actors[f.actor.ID] = f.actor
	f.mu.Unlock()
	if response := f.request(t, f.actor, "POST", "transfers", transferFetchRequest, nil); response.Code != 403 {
		t.Fatalf("read-only fetch=%d", response.Code)
	}
	if f.fetcher.calls() != 0 || len(f.storage.uploadSnapshot()) != 0 {
		t.Fatal("refused request started work")
	}
}

func TestTransferSubmissionRejectsNamespaceChangeDuringMetadataLookup(t *testing.T) {
	for _, test := range []struct {
		name, body, metadataPath string
	}{
		{"fetch", transferFetchRequest, "/target"},
		{"archive", `{"kind":"archive","paths":["/source"]}`, "/source"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newTransferFixture(t)
			// Distinct spaces can reuse an entry ID, especially the root "0".
			// A namespace change must be caught even when that ID stays equal.
			f.storage.replaceID(test.metadataPath, "0")
			var switched sync.Once
			f.storage.afterMetadata = func(path string) {
				if path == test.metadataPath {
					switched.Do(func() {
						f.mu.Lock()
						f.identity = strings.Repeat("d", 64)
						f.mu.Unlock()
					})
				}
			}
			response := f.request(t, f.actor, "POST", "transfers", test.body, nil)
			if response.Code != 403 {
				t.Fatalf("changed namespace accepted: %d %s", response.Code, response.Body.String())
			}
			tasks, _ := f.controller.manager.ListOwned(f.actor.ID, f.actor.PolicyVersion)
			if len(tasks) != 0 || f.fetcher.calls() != 0 || len(f.storage.uploadSnapshot()) != 0 {
				t.Fatal("namespace change started a transfer")
			}
		})
	}
}

func TestTransferCancelDuringFetchNeverUploads(t *testing.T) {
	f := newTransferFixture(t)
	started := make(chan struct{})
	f.fetcher.before = func(ctx context.Context, _ io.Writer) error { close(started); <-ctx.Done(); return ctx.Err() }
	created := f.submit(t, transferFetchRequest)
	<-started
	response := f.request(t, f.actor, "POST", "transfers/"+created.ID+"/cancel", "", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	final := f.wait(t, created.ID)
	if final.State != "cancelled" || !final.CanRetry || len(f.storage.uploadSnapshot()) != 0 {
		t.Fatalf("task=%+v uploads=%v", final, f.storage.uploadSnapshot())
	}
	f.assertReleased(t, created.ID)
}

func TestTransferCancelAfterDownloadBeforeUploadNeverUploads(t *testing.T) {
	f := newTransferFixture(t)
	downloaded, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f.fetcher.after = func() { close(downloaded); <-release }
	created := f.submit(t, transferFetchRequest)
	<-downloaded
	response := f.request(t, f.actor, "POST", "transfers/"+created.ID+"/cancel", "", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	unblock()
	final := f.wait(t, created.ID)
	if final.State != "cancelled" || !final.CanRetry || len(f.storage.uploadSnapshot()) != 0 {
		t.Fatalf("task=%+v uploads=%v", final, f.storage.uploadSnapshot())
	}
	f.assertReleased(t, created.ID)
}

func TestTransferUploadUncertaintyCannotCancelOrRetry(t *testing.T) {
	f := newTransferFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	f.storage.onUpload = func() { close(started); <-release }
	f.storage.uploadErr = errors.New("upstream signed_url=secret")
	created := f.submit(t, transferFetchRequest)
	<-started
	response := f.request(t, f.actor, "POST", "transfers/"+created.ID+"/cancel", "", nil)
	if response.Code != 409 {
		t.Fatal(response.Code, response.Body.String())
	}
	close(release)
	final := f.wait(t, created.ID)
	if final.State != "interrupted" || final.CanRetry || final.CanCancel || strings.Contains(final.Error, "signed_url") {
		t.Fatalf("uncertain upload=%+v", final)
	}
	if response := f.request(t, f.actor, "POST", "transfers/"+created.ID+"/retry", "", nil); response.Code != 409 {
		t.Fatal("uncertain retry accepted", response.Code)
	}
	f.assertReleased(t, created.ID)
}

func TestTransferFetchRefusesDestinationReplacementBeforeUpload(t *testing.T) {
	for _, replace := range []string{"parent", "new target"} {
		t.Run(replace, func(t *testing.T) {
			f := newTransferFixture(t)
			f.fetcher.after = func() {
				if replace == "parent" {
					f.storage.replaceID("/target", "replacement-parent")
				} else {
					f.storage.mu.Lock()
					f.storage.entries["/target/new.bin"] = archiveFile("new-owner-file", "new.bin", "private")
					f.storage.mu.Unlock()
				}
			}
			created := f.submit(t, transferFetchRequest)
			final := f.wait(t, created.ID)
			if final.State == "completed" || len(f.storage.uploadSnapshot()) != 0 {
				t.Fatalf("replaced destination uploaded: %+v", final)
			}
			f.assertReleased(t, created.ID)
		})
	}
}

func TestTransferArchiveArtifactAndOwnerIsolation(t *testing.T) {
	f := newTransferFixture(t)
	created := f.submit(t, `{"kind":"archive","paths":["/source"]}`)
	final := f.wait(t, created.ID)
	if final.State != "completed" || !final.ArtifactReady {
		t.Fatal(final)
	}
	response := f.request(t, f.actor, "GET", "transfers/"+created.ID+"/download", "", nil)
	if response.Code != 200 || response.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("artifact=%d %s", response.Code, response.Body.String())
	}
	reader, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) != 2 || reader.File[1].Name != "source/literal%25 + 文.txt" {
		t.Fatal(reader.File)
	}
	file, err := reader.File[1].Open()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	file.Close()
	if err != nil || string(data) != "archive-content" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	ranged := f.request(t, f.actor, "GET", "transfers/"+created.ID+"/download", "", map[string]string{"Range": "bytes=0-3"})
	if ranged.Code != 206 || !bytes.Equal(ranged.Body.Bytes(), response.Body.Bytes()[:4]) {
		t.Fatalf("range=%d %q", ranged.Code, ranged.Body.String())
	}
	bob := f.actor
	bob.ID = strings.Repeat("b", 32)
	bob.Username = "bob"
	f.mu.Lock()
	f.actors[bob.ID] = bob
	f.mu.Unlock()
	for _, suffix := range []string{created.ID, created.ID + "/download"} {
		other := f.request(t, bob, "GET", "transfers/"+suffix, "", nil)
		if other.Code != 404 {
			t.Fatalf("foreign artifact=%d %s", other.Code, other.Body.String())
		}
	}
	updated := f.actor
	updated.PolicyVersion++
	f.mu.Lock()
	f.actors[updated.ID] = updated
	f.mu.Unlock()
	if response := f.request(t, updated, "GET", "transfers/"+created.ID+"/download", "", nil); response.Code != 404 {
		t.Fatalf("new-policy read=%d %s", response.Code, response.Body.String())
	}
	f.assertReleased(t, created.ID)
}

func TestTransferArchiveHandlesUnknownSourceSizes(t *testing.T) {
	f := newTransferFixture(t)
	body := strings.Repeat("image-bytes", 4096)
	f.storage.mu.Lock()
	entry := f.storage.entries["/source/literal%25 + 文.txt"]
	entry.Size = nil
	f.storage.entries["/source/literal%25 + 文.txt"] = entry
	f.storage.children["/source"] = []model.RemoteEntry{entry}
	f.storage.data["/source/literal%25 + 文.txt"] = body
	f.storage.mu.Unlock()
	created := f.submit(t, `{"kind":"archive","paths":["/source"]}`)
	final := f.wait(t, created.ID)
	if final.State != "completed" || !final.ArtifactReady {
		t.Fatalf("unknown-size archive failed: %+v", final)
	}
	if final.BytesTotal != 0 && final.BytesDone > final.BytesTotal {
		t.Fatalf("estimated total became an invalid hard cap: %+v", final)
	}
	f.assertReleased(t, created.ID)
}

func TestTransferArtifactDownloadHonorsSharedDownloadBudget(t *testing.T) {
	f := newTransferFixture(t)
	created := f.submit(t, `{"kind":"archive","paths":["/source"]}`)
	if final := f.wait(t, created.ID); !final.ArtifactReady {
		t.Fatal(final)
	}
	release, err := f.budget.AcquireDownload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if response := f.request(t, f.actor, "GET", "transfers/"+created.ID+"/download", "", nil); response.Code != 503 {
		t.Fatalf("busy download=%d %s", response.Code, response.Body.String())
	}
	if active := f.budget.Stats().DownloadsActive; active != 1 {
		t.Fatalf("download wait changed active count: %d", active)
	}
	release()
	if response := f.request(t, f.actor, "GET", "transfers/"+created.ID+"/download", "", nil); response.Code != 200 {
		t.Fatalf("released download=%d %s", response.Code, response.Body.String())
	}
	f.assertReleased(t, created.ID)
}

func TestTransferArchiveRejectsChangedSourceAndCleansPartialFile(t *testing.T) {
	f := newTransferFixture(t)
	f.storage.onOpen = func(string) { f.storage.replaceID("/source", "replacement-folder") }
	created := f.submit(t, `{"kind":"archive","paths":["/source"]}`)
	final := f.wait(t, created.ID)
	if final.State == "completed" || final.ArtifactReady || final.CanRetry {
		t.Fatalf("changed source accepted: %+v", final)
	}
	if _, err := os.Stat(f.controller.filename(created.ID, ".zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial artifact remains: %v", err)
	}
	f.assertReleased(t, created.ID)
}

func TestTransferArtifactRejectsMissingFileAndSymlink(t *testing.T) {
	for _, mode := range []string{"missing", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			f := newTransferFixture(t)
			created := f.submit(t, `{"kind":"archive","paths":["/source"]}`)
			if final := f.wait(t, created.ID); !final.ArtifactReady {
				t.Fatal(final)
			}
			artifact := f.controller.filename(created.ID, ".zip")
			if err := os.Remove(artifact); err != nil {
				t.Fatal(err)
			}
			if mode == "symlink" {
				private := filepath.Join(filepath.Dir(f.config.Directory), "unrelated-secret")
				if err := os.WriteFile(private, []byte("private file contents"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(private, artifact); err != nil {
					t.Fatal(err)
				}
			}
			response := f.request(t, f.actor, "GET", "transfers/"+created.ID+"/download", "", nil)
			if response.Code != 404 || strings.Contains(response.Body.String(), "private file contents") {
				t.Fatalf("artifact protection=%d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestTransferArtifactExpiryPrunesFileAfterRestart(t *testing.T) {
	f := newTransferFixture(t)
	created := f.submit(t, `{"kind":"archive","paths":["/source"]}`)
	if final := f.wait(t, created.ID); !final.ArtifactReady {
		t.Fatal(final)
	}
	f.controller.Close()
	raw, err := os.ReadFile(f.config.File)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	for _, value := range state["records"].([]any) {
		record := value.(map[string]any)
		if record["task"].(map[string]any)["id"] == created.ID {
			record["artifact"].(map[string]any)["expires_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		}
	}
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config.File, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f.controller, err = NewTransferController(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if response := f.request(t, f.actor, "GET", "transfers/"+created.ID+"/download", "", nil); response.Code != 404 {
		t.Fatalf("expired artifact=%d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(f.controller.filename(created.ID, ".zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired archive not pruned: %v", err)
	}
}

func TestTransferWriterReservesEveryWrittenByteAndHonorsDiskQuota(t *testing.T) {
	for _, estimated := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "declared overflow"}[estimated], func(t *testing.T) {
			f := newTransferFixture(t)
			id := strings.Repeat("d", 32)
			writer, err := f.controller.newWriter(context.Background(), id, 1<<20, func(transfers.Progress) error { return nil }, "packaging")
			if err != nil {
				t.Fatal(err)
			}
			defer writer.close()
			expected := int64(-1)
			if estimated {
				expected = 2
			}
			if err := writer.SetExpectedSize(expected); err != nil {
				t.Fatal(err)
			}
			if estimated {
				if n, err := writer.Write([]byte("1234")); err == nil || n != 0 || writer.written != 0 {
					t.Fatalf("declared length exceeded: n=%d written=%d err=%v", n, writer.written, err)
				}
			} else {
				if _, err := writer.Write([]byte("1234")); err != nil {
					t.Fatal(err)
				}
				if reserved := f.budget.Stats().SpoolReservedBytes; reserved < 4 {
					t.Fatalf("reserved=%d below written=4", reserved)
				}
				if _, err := writer.Write([]byte("5678")); err != nil {
					t.Fatal(err)
				}
				if reserved := f.budget.Stats().SpoolReservedBytes; reserved < 8 {
					t.Fatalf("reserved=%d below written=8", reserved)
				}
			}
			writer.close()
			if err := f.controller.removeSuffix(id, ".part"); err != nil {
				t.Fatal(err)
			}
			fill, err := os.OpenFile(filepath.Join(f.config.Directory, "quota-fill"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err := fill.Truncate(transferDiskLimit - 6); err != nil {
				t.Fatal(err)
			}
			fill.Close()
			writer, err = f.controller.newWriter(context.Background(), strings.Repeat("e", 32), 1<<20, func(transfers.Progress) error { return nil }, "downloading")
			if err != nil {
				t.Fatal(err)
			}
			defer writer.close()
			if err := writer.SetExpectedSize(-1); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte("1234")); err != nil {
				return
			} // Reserving a complete chunk may refuse earlier.
			if _, err := writer.Write([]byte("5678")); err == nil {
				t.Fatal("writer exceeded aggregate disk quota")
			}
		})
	}
}

func TestTransferStatePersistenceFailureStopsBeforeUpload(t *testing.T) {
	for _, stage := range []string{"download", "upload barrier"} {
		t.Run(stage, func(t *testing.T) {
			f := newTransferFixture(t)
			breakJournal := func() error {
				if err := os.Remove(f.config.File); err != nil {
					return err
				}
				return os.Mkdir(f.config.File, 0700)
			}
			if stage == "download" {
				f.fetcher.before = func(context.Context, io.Writer) error { return breakJournal() }
			} else {
				f.fetcher.after = func() {
					if err := breakJournal(); err != nil {
						t.Error(err)
					}
				}
			}
			created := f.submit(t, transferFetchRequest)
			final := f.wait(t, created.ID)
			if final.State == "completed" || final.CanRetry || len(f.storage.uploadSnapshot()) != 0 {
				t.Fatalf("unsaved transfer continued: %+v", final)
			}
			_, failed := f.controller.manager.ListOwned(f.actor.ID, f.actor.PolicyVersion)
			if !failed {
				t.Fatal("persistence failure not exposed")
			}
			f.assertReleased(t, created.ID)
		})
	}
}
