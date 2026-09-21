package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

type textEditStore struct {
	mu                           sync.Mutex
	entry                        model.RemoteEntry
	body                         []byte
	invalidations, writes, reads int
	uploadErr                    error
	writeDifferent               bool
	started, release             chan struct{}
	stream                       DownloadStream
}

func newTextEditStore() *textEditStore {
	return &textEditStore{entry: model.RemoteEntry{ID: "existing-file", Name: "draft.txt", Kind: model.KindFile}, body: []byte("original")}
}

func (s *textEditStore) Metadata(path string) (model.RemoteEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry.ID == "" {
		return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "missing")
	}
	entry := s.entry
	entry.Size = model.Ptr(int64(len(s.body)))
	return entry, nil
}
func (s *textEditStore) ListPath(string) ([]model.RemoteEntry, error) { return nil, nil }
func (s *textEditStore) ListChildren(string, model.RemoteEntry) ([]model.RemoteEntry, error) {
	return nil, nil
}
func (s *textEditStore) InvalidateMetadataCache() { s.mu.Lock(); s.invalidations++; s.mu.Unlock() }
func (s *textEditStore) OpenPath(ctx context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.stream != nil {
		return s.stream, nil
	}
	return newFakeStream(string(s.body), model.Ptr(int64(len(s.body)))), nil
}
func (s *textEditStore) UploadPath(ctx context.Context, path string, reader io.Reader, options storage.UploadOptions) (model.RemoteEntry, error) {
	if s.started != nil {
		close(s.started)
		<-s.release
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if options.ExpectedID == "" || options.ExpectedID != s.entry.ID || !options.Overwrite {
		return model.RemoteEntry{}, storage.ErrUploadTargetChanged
	}
	s.writes++
	s.body = body
	if s.writeDifferent {
		s.body = []byte("different remote content")
	}
	return s.entry, s.uploadErr
}

func textEditHarness(t *testing.T) (*RESTDispatcher, *Router, *textEditStore) {
	t.Helper()
	store := newTextEditStore()
	dispatcher := newReadDispatcherDownloads(t, store, nil, store)
	dispatcher.uploads = store
	return dispatcher, readRouter(t, dispatcher), store
}
func editRequest(router *Router, method, path, revision string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/api/v1/text?path="+path, bytes.NewReader(body))
	if revision != "" {
		request.Header.Set("If-Match", revision)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestTextEditReadsLegacyBytesAndSavesRepeatably(t *testing.T) {
	_, router, store := textEditHarness(t)
	store.body = []byte{0xc4, 0xe3, 0xba, 0xc3}
	first := editRequest(router, "GET", "/draft.txt", "", nil)
	if first.Code != 200 || !bytes.Equal(first.Body.Bytes(), store.body) || first.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("legacy read: %d %q", first.Code, first.Body.String())
	}
	revision := first.Header().Get("ETag")
	if len(revision) != 66 || strings.HasPrefix(revision, "W/") {
		t.Fatalf("revision = %q", revision)
	}
	for _, body := range []string{"你好\n", "second save", ""} {
		recorder := editRequest(router, "PUT", "/draft.txt", revision, []byte(body))
		var response struct {
			Revision string `json:"revision"`
		}
		if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &response) != nil || response.Revision == revision || response.Revision != recorder.Header().Get("ETag") {
			t.Fatalf("save: %d %s", recorder.Code, recorder.Body.String())
		}
		revision = response.Revision
		read := editRequest(router, "GET", "/draft.txt", "", nil)
		if read.Body.String() != body || read.Header().Get("ETag") != revision {
			t.Fatal("save verification or revision unstable")
		}
	}
	if store.invalidations < 12 {
		t.Fatal("editor reused stale metadata")
	}
}

func TestTextEditRejectsConflictingContentPathIdentityAndReplacement(t *testing.T) {
	for _, conflict := range []string{"content", "path", "identity", "replacement", "missing", "unavailable identity"} {
		t.Run(conflict, func(t *testing.T) {
			dispatcher, router, store := textEditHarness(t)
			var identity atomic.Uint32
			dispatcher.SetSearchIdentity(func() ([32]byte, error) {
				if identity.Load() == 255 {
					return [32]byte{}, errors.New("private source details")
				}
				return [32]byte{byte(identity.Load())}, nil
			})
			revision := editRequest(router, "GET", "/draft.txt", "", nil).Header().Get("ETag")
			path := "/draft.txt"
			switch conflict {
			case "content":
				store.body = []byte("external revision")
			case "path":
				path = "/another.txt"
			case "identity":
				identity.Add(1)
			case "replacement":
				store.entry.ID = "replacement-file"
			case "missing":
				store.entry.ID = ""
			case "unavailable identity":
				identity.Store(255)
			}
			recorder := editRequest(router, "PUT", path, revision, []byte("new"))
			if recorder.Code != 412 || store.writes != 0 || strings.Contains(recorder.Body.String(), "private") {
				t.Fatalf("conflict = %d %s writes=%d", recorder.Code, recorder.Body.String(), store.writes)
			}
		})
	}
}

func TestTextEditRejectsUnsafeOrOversizedInputs(t *testing.T) {
	tests := []struct {
		name     string
		body     []byte
		revision string
		want     int
		maximum  int64
	}{
		{"missing precondition", []byte("new"), "", 428, 0},
		{"wildcard", []byte("new"), "*", 412, 0},
		{"weak", []byte("new"), `W/"` + strings.Repeat("a", 64) + `"`, 412, 0},
		{"invalid UTF8", []byte{0xff}, "valid", 400, 0},
		{"editor limit", bytes.Repeat([]byte("x"), int(textEditMaxBytes+1)), "valid", 413, 0},
		{"upload budget", []byte("1234"), "valid", 507, 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dispatcher, router, store := textEditHarness(t)
			dispatcher.maxUploadBytes = test.maximum
			revision := test.revision
			if revision == "valid" {
				revision = editRequest(router, "GET", "/draft.txt", "", nil).Header().Get("ETag")
			}
			recorder := editRequest(router, "PUT", "/draft.txt", revision, test.body)
			if recorder.Code != test.want || store.writes != 0 {
				t.Fatalf("result: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	_, router, store := textEditHarness(t)
	store.body = bytes.Repeat([]byte("x"), int(textEditMaxBytes+1))
	if recorder := editRequest(router, "GET", "/draft.txt", "", nil); recorder.Code != 413 || store.reads != 0 {
		t.Fatal("oversize metadata still opened download")
	}
	store.body = []byte("x")
	store.entry.Name = "photo.jpg"
	if recorder := editRequest(router, "GET", "/photo.jpg", "", nil); recorder.Code != 501 {
		t.Fatal("binary format offered for editing")
	}
}

func TestTextEditHonorsDAVLocks(t *testing.T) {
	dispatcher, router, store := textEditHarness(t)
	revision := editRequest(router, "GET", "/draft.txt", "", nil).Header().Get("ETag")
	lock, err := dispatcher.locks.Acquire("/", "infinity", "", 30, "")
	if err != nil {
		t.Fatal(err)
	}
	if recorder := editRequest(router, "PUT", "/draft.txt", revision, []byte("changed")); recorder.Code != 423 || store.writes != 0 {
		t.Fatal("locked file overwritten")
	}
	request := httptest.NewRequest("PUT", "/api/v1/text?path=/draft.txt", strings.NewReader("changed"))
	request.Header.Set("If-Match", revision)
	request.Header.Set("Lock-Token", "<"+lock.Token+">")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("lock owner cannot save: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestTextEditSerializesConcurrentSaves(t *testing.T) {
	_, router, store := textEditHarness(t)
	revision := editRequest(router, "GET", "/draft.txt", "", nil).Header().Get("ETag")
	done := make(chan int, 2)
	for _, contents := range []string{"save one", "save two"} {
		go func() { done <- editRequest(router, "PUT", "/draft.txt", revision, []byte(contents)).Code }()
	}
	first, second := <-done, <-done
	if !((first == 200 && second == 412) || (first == 412 && second == 200)) || store.writes != 1 {
		t.Fatalf("concurrent saves: %d/%d, %d writes", first, second, store.writes)
	}
}

func TestTextEditCancellationWhileQueuedDoesNotUpload(t *testing.T) {
	dispatcher, router, store := textEditHarness(t)
	revision := editRequest(router, "GET", "/draft.txt", "", nil).Header().Get("ETag")
	release, err := dispatcher.editorState().acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("PUT", "/api/v1/text?path=/draft.txt", strings.NewReader("new")).WithContext(ctx)
	request.Header.Set("If-Match", revision)
	done := make(chan struct{})
	go func() { router.ServeHTTP(httptest.NewRecorder(), request); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queued editor ignored cancellation")
	}
	if store.writes != 0 {
		t.Fatal("cancelled request uploaded")
	}
}

func TestTextEditReportsUncertainPersistenceWithoutRetry(t *testing.T) {
	for _, changed := range []bool{false, true} {
		_, router, store := textEditHarness(t)
		revision := editRequest(router, "GET", "/draft.txt", "", nil).Header().Get("ETag")
		if changed {
			store.writeDifferent = true
		} else {
			store.uploadErr = errors.New("signed-url-and-secret")
		}
		recorder := editRequest(router, "PUT", "/draft.txt", revision, []byte("new"))
		want, code := 502, "text_save_uncertain"
		if changed {
			want, code = 409, "text_save_unverified"
		}
		if recorder.Code != want || !strings.Contains(recorder.Body.String(), code) || strings.Contains(recorder.Body.String(), "secret") || store.writes != 1 || recorder.Header().Get("ETag") != "" {
			t.Fatalf("uncertain save: %d %s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestTextEditBoundsActualStreamAndRejectsIncompleteDownload(t *testing.T) {
	for _, test := range []struct {
		name, body string
		length     *int64
		want       int
	}{
		{"actual limit", strings.Repeat("x", int(textEditMaxBytes+1)), nil, 413},
		{"truncated", "short", model.Ptr(int64(100)), 502},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, router, store := textEditHarness(t)
			store.stream = newFakeStream(test.body, test.length)
			recorder := editRequest(router, "GET", "/draft.txt", "", nil)
			if recorder.Code != test.want || recorder.Header().Get("ETag") != "" {
				t.Fatalf("incomplete read: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestTextEditCancelsBlockedDownload(t *testing.T) {
	_, router, store := textEditHarness(t)
	reader, writer := io.Pipe()
	defer writer.Close()
	stream := &cancelBlockingStream{fakeDownloadStream: newFakeStream("", nil), reader: reader, entered: make(chan struct{})}
	store.stream = stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("GET", "/api/v1/text?path=/draft.txt", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { router.ServeHTTP(httptest.NewRecorder(), request); close(done) }()
	select {
	case <-stream.entered:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not close blocked editor stream")
	}
}

func TestTextEditIdentityChangeWhileWaitingToUploadStopsWriter(t *testing.T) {
	dispatcher, router, store := textEditHarness(t)
	var identity atomic.Uint32
	dispatcher.SetSearchIdentity(func() ([32]byte, error) { return [32]byte{byte(identity.Load())}, nil })
	revision := editRequest(router, "GET", "/draft.txt", "", nil).Header().Get("ETag")
	store.started, store.release = make(chan struct{}), make(chan struct{})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- editRequest(router, "PUT", "/draft.txt", revision, []byte("new")) }()
	<-store.started
	identity.Add(1)
	close(store.release)
	if recorder := <-done; recorder.Code != 412 || store.writes != 0 {
		t.Fatalf("old identity reached writer: %d %s", recorder.Code, recorder.Body.String())
	}
}
