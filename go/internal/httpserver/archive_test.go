package httpserver

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

type archiveStorageFake struct {
	entries  map[string]model.RemoteEntry
	children map[string][]model.RemoteEntry
	streams  map[string]DownloadStream
	listErr  error
	openErr  error
	opened   []string
}

func (f *archiveStorageFake) Metadata(path string) (model.RemoteEntry, error) {
	if e, ok := f.entries[path]; ok {
		return e, nil
	}
	return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "entry not found")
}
func (f *archiveStorageFake) ListPath(path string) ([]model.RemoteEntry, error) {
	return f.children[path], f.listErr
}
func (f *archiveStorageFake) ListChildren(path string, _ model.RemoteEntry) ([]model.RemoteEntry, error) {
	return f.children[path], f.listErr
}
func (f *archiveStorageFake) OpenPath(_ context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	if offset != 0 || length != nil {
		panic("archive must request whole files")
	}
	f.opened = append(f.opened, path)
	return f.streams[path], f.openErr
}

func archiveFile(id, name, text string) model.RemoteEntry {
	e := fileEntry(id, name)
	e.Size = model.Ptr(int64(len(text)))
	return e
}

func archiveFolder(id, name string) model.RemoteEntry {
	e := folderEntry(id)
	e.Name = name
	return e
}

func archiveDispatcher(t *testing.T, f *archiveStorageFake) *RESTDispatcher {
	return newReadDispatcherDownloads(t, f, nil, f)
}

type replacingArchiveSource struct{ *archiveStorageFake }

func (f replacingArchiveSource) OpenPath(ctx context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	stream, err := f.archiveStorageFake.OpenPath(ctx, path, offset, length)
	replacement := f.entries[path]
	replacement.ID = "replacement"
	f.entries[path] = replacement
	return stream, err
}

func TestArchiveRejectsReplacementWhileOpeningStream(t *testing.T) {
	entry := archiveFile("original", "a.txt", "secret")
	stream := newFakeStream("secret", entry.Size)
	fake := &archiveStorageFake{entries: map[string]model.RemoteEntry{"/a.txt": entry}, streams: map[string]DownloadStream{"/a.txt": stream}}
	d := newReadDispatcherDownloads(t, fake, nil, replacingArchiveSource{fake})
	var output bytes.Buffer
	_, err := d.copyArchiveFile(context.Background(), &output, archiveItem{path: "/a.txt", name: "a.txt", entry: entry}, 100, make([]byte, 32))
	if err == nil || output.Len() != 0 || stream.closeCalls() != 1 {
		t.Fatalf("replacement streamed: err=%v bytes=%d closes=%d", err, output.Len(), stream.closeCalls())
	}
}

func TestArchiveStreamsNestedFilesEmptyFoldersAndLiteralNames(t *testing.T) {
	for _, transport := range []string{"json", "form", "get"} {
		t.Run(transport, func(t *testing.T) {
			root := archiveFolder("folder", "资料")
			empty := archiveFolder("empty", "empty")
			file := archiveFile("file", "100%25 + 笔记.txt", "hello archive")
			stream := newFakeStream("hello archive", file.Size)
			fake := &archiveStorageFake{
				entries:  map[string]model.RemoteEntry{"/资料": root, "/资料/100%25 + 笔记.txt": file},
				children: map[string][]model.RemoteEntry{"/资料": {empty, file}},
				streams:  map[string]DownloadStream{"/资料/100%25 + 笔记.txt": stream},
			}
			var request *http.Request
			switch transport {
			case "json":
				request = httptest.NewRequest("POST", "/api/v1/archive", strings.NewReader(`{"paths":["/资料"]}`))
			case "form":
				request = httptest.NewRequest("POST", "/api/v1/archive", strings.NewReader(url.Values{"paths": {`["/资料"]`}}.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			case "get":
				request = httptest.NewRequest("GET", "/api/v1/archive?"+url.Values{"path": {"/资料"}}.Encode(), nil)
			}
			if request.Method == "POST" {
				request.Header.Set("Content-Length", strconv.FormatInt(request.ContentLength, 10))
			}
			w := httptest.NewRecorder()
			readRouter(t, archiveDispatcher(t, fake)).ServeHTTP(w, request)
			if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" || !strings.Contains(w.Header().Get("Content-Disposition"), ".zip") {
				t.Fatalf("code=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
			}
			reader, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, item := range reader.File {
				names = append(names, item.Name)
				if item.FileInfo().IsDir() {
					continue
				}
				body, err := item.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(body)
				body.Close()
				if err != nil || string(data) != "hello archive" {
					t.Fatalf("data=%q err=%v", data, err)
				}
			}
			if !reflect.DeepEqual(names, []string{"资料/", "资料/empty/", "资料/100%25 + 笔记.txt"}) {
				t.Fatal(names)
			}
			if stream.closeCalls() != 1 || !reflect.DeepEqual(fake.opened, []string{"/资料/100%25 + 笔记.txt"}) {
				t.Fatalf("close=%d opened=%v", stream.closeCalls(), fake.opened)
			}
		})
	}
}

func TestArchivePreflightRejectsInvalidTreesWithoutOpeningFiles(t *testing.T) {
	for _, scenario := range []string{"duplicate", "case collision", "traversal", "embedded slash", "too big", "cycle", "too many", "list failure", "unknown kind", "depth"} {
		t.Run(scenario, func(t *testing.T) {
			root := archiveFolder("root", "folder")
			file := archiveFile("file", "a.txt", "a")
			fake := &archiveStorageFake{entries: map[string]model.RemoteEntry{"/folder": root}, children: map[string][]model.RemoteEntry{"/folder": {file}}}
			switch scenario {
			case "duplicate":
				fake.children["/folder"] = []model.RemoteEntry{file, file}
			case "case collision":
				duplicate := file
				duplicate.Name = "A.TXT"
				fake.children["/folder"] = []model.RemoteEntry{file, duplicate}
			case "traversal":
				file.Name = ".."
				fake.children["/folder"] = []model.RemoteEntry{file}
			case "embedded slash":
				file.Name = "a/b"
				fake.children["/folder"] = []model.RemoteEntry{file}
			case "too big":
				file.Size = model.Ptr(archiveMaxBytes + 1)
				fake.children["/folder"] = []model.RemoteEntry{file}
			case "cycle":
				fake.children["/folder"] = []model.RemoteEntry{root}
			case "too many":
				fake.children["/folder"] = make([]model.RemoteEntry, archiveMaxEntries)
			case "list failure":
				fake.listErr = model.NewWpsAPIError("list", 503, model.WpsCategoryHTTP)
			case "unknown kind":
				file.Kind = model.KindUnknown
				fake.children["/folder"] = []model.RemoteEntry{file}
			case "depth":
				at := "/folder"
				for i := 0; i <= archiveMaxDepth; i++ {
					child := archiveFolder(at, "sub")
					fake.children[at] = []model.RemoteEntry{child}
					at += "/sub"
				}
			}
			w := httptest.NewRecorder()
			readRouter(t, archiveDispatcher(t, fake)).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/archive?path=/folder", nil))
			if w.Code < 400 || w.Header().Get("Content-Type") != contentTypeJSON || len(fake.opened) != 0 {
				t.Fatalf("status=%d opened=%v body=%s", w.Code, fake.opened, w.Body.String())
			}
		})
	}
}

func TestArchiveRejectsOversizedAndMalformedSelections(t *testing.T) {
	paths := make([]string, maxSelectionPaths+1)
	for i := range paths {
		paths[i] = "/file"
	}
	tooMany, _ := json.Marshal(map[string]any{"paths": paths})
	for _, body := range []string{`{"paths":["/a","/a/b"]}`, `{"paths":["/"]}`, `{"paths":["/a"],"unknown":true}`, string(tooMany)} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/v1/archive", strings.NewReader(body))
		r.Header.Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
		readRouter(t, archiveDispatcher(t, &archiveStorageFake{})).ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
}

func TestArchiveStreamFailuresAbortHTTPWithoutCompleteZIP(t *testing.T) {
	for _, scenario := range []string{"short", "long", "length changed", "read error", "open error", "changed entry"} {
		t.Run(scenario, func(t *testing.T) {
			file := archiveFile("file", "file.txt", "expected")
			stream := newFakeStream("expected", file.Size)
			fake := &archiveStorageFake{entries: map[string]model.RemoteEntry{"/file.txt": file}, streams: map[string]DownloadStream{"/file.txt": stream}}
			switch scenario {
			case "short":
				stream.data = strings.NewReader("tiny")
			case "long":
				stream.data = strings.NewReader("expected plus extra")
			case "length changed":
				stream.contentLength = model.Ptr(int64(12))
			case "read error":
				stream.readErrs = []error{errors.New("upstream read failed")}
			case "open error":
				fake.openErr = errors.New("upstream unavailable")
			case "changed entry":
				file.Etag = model.Ptr("different")
			}
			d := archiveDispatcher(t, fake)
			plan := &archivePlan{items: []archiveItem{{path: "/file.txt", name: "file.txt", entry: file}}}
			w := httptest.NewRecorder()
			if err := d.writeArchive(w, httptest.NewRequest("GET", "/", nil), plan); err == nil {
				t.Fatal("expected transfer error")
			}
			if _, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len())); err == nil {
				t.Fatal("failure produced a complete ZIP")
			}
			if scenario != "open error" && scenario != "changed entry" && stream.closeCalls() != 1 {
				t.Fatalf("closes=%d", stream.closeCalls())
			}
		})
	}
}

func TestArchiveAbortsTransportThroughPanicMiddleware(t *testing.T) {
	good := archiveFile("good", "good.txt", strings.Repeat("x", 8192))
	file := archiveFile("file", "file.txt", "expected")
	stream := newFakeStream("short", file.Size)
	fake := &archiveStorageFake{
		entries: map[string]model.RemoteEntry{"/good.txt": good, "/file.txt": file},
		streams: map[string]DownloadStream{"/good.txt": newFakeStream(strings.Repeat("x", 8192), good.Size), "/file.txt": stream},
	}
	handler := recoverPanics(nil)(readRouter(t, archiveDispatcher(t, fake)))
	server := httptest.NewServer(handler)
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/api/v1/archive?path=/good.txt&path=/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	partial, err := io.ReadAll(response.Body)
	if err == nil {
		t.Fatal("truncated archive appeared as a successful HTTP transfer")
	}
	if len(partial) < 8192 {
		t.Fatal("expected failure after the first file was streamed")
	}
}

func TestArchiveBoundsUnknownLengthStreams(t *testing.T) {
	file := archiveFile("file", "file", "")
	file.Size = nil
	stream := newFakeStream("longer than budget", nil)
	fake := &archiveStorageFake{entries: map[string]model.RemoteEntry{"/file": file}, streams: map[string]DownloadStream{"/file": stream}}
	d := archiveDispatcher(t, fake)
	output := &bytes.Buffer{}
	written, err := d.copyArchiveFile(context.Background(), output, archiveItem{path: "/file", entry: file}, 3, make([]byte, 1024))
	if err == nil || written != 4 || output.Len() != 4 || stream.closeCalls() != 1 {
		t.Fatalf("unknown-size limit not enforced: written=%d len=%d closes=%d error=%v", written, output.Len(), stream.closeCalls(), err)
	}
}

type blockingArchiveStream struct {
	started   chan struct{}
	closed    chan struct{}
	once      sync.Once
	startOnce sync.Once
}

func (s *blockingArchiveStream) Read([]byte) (int, error) {
	s.startOnce.Do(func() { close(s.started) })
	<-s.closed
	return 0, errors.New("closed")
}
func (s *blockingArchiveStream) Close() error          { s.once.Do(func() { close(s.closed) }); return nil }
func (s *blockingArchiveStream) HTTPStatus() int       { return 200 }
func (s *blockingArchiveStream) ContentType() *string  { return nil }
func (s *blockingArchiveStream) ContentLength() *int64 { return nil }
func (s *blockingArchiveStream) ContentRange() *string { return nil }

func TestArchiveCancellationClosesBlockedUpstream(t *testing.T) {
	file := archiveFile("file", "file", "expected")
	stream := &blockingArchiveStream{started: make(chan struct{}), closed: make(chan struct{})}
	fake := &archiveStorageFake{entries: map[string]model.RemoteEntry{"/file": file}, streams: map[string]DownloadStream{"/file": stream}}
	d := archiveDispatcher(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := d.copyArchiveFile(ctx, io.Discard, archiveItem{path: "/file", entry: file}, archiveMaxBytes, make([]byte, 1024))
		done <- err
	}()
	select {
	case <-stream.started:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled archive succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not release stream")
	}
}
