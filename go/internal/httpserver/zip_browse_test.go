package httpserver

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

type zipFixtureFile struct {
	name   string
	body   []byte
	method uint16
	mode   fs.FileMode
}

func zipFixture(t *testing.T, files ...zipFixtureFile) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, file := range files {
		header := &zip.FileHeader{Name: file.name, Method: file.method}
		if file.mode != 0 {
			header.SetMode(file.mode)
		}
		stream, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = stream.Write(file.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

type zipFixtureStream struct {
	io.ReadCloser
	offset, length, total int64
	status                int
}

func (s *zipFixtureStream) HTTPStatus() int       { return s.status }
func (s *zipFixtureStream) ContentType() *string  { return model.Ptr("application/zip") }
func (s *zipFixtureStream) ContentLength() *int64 { return &s.length }
func (s *zipFixtureStream) ContentRange() *string {
	return model.Ptr(fmt.Sprintf("bytes %d-%d/%d", s.offset, s.offset+s.length-1, s.total))
}

type zipFixtureStorage struct {
	mu           sync.Mutex
	body         []byte
	ranges       [][2]int64
	metaSize     *int64
	status       int
	changed      bool
	changeOnOpen bool
	block        *io.PipeReader
	entered      chan struct{}
}

func (s *zipFixtureStorage) Metadata(string) (model.RemoteEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	size := int64(len(s.body))
	if s.metaSize != nil {
		size = *s.metaSize
	}
	etag := "original"
	if s.changed {
		etag = "changed"
	}
	return model.RemoteEntry{ID: "zip-file", Name: "fixture.zip", Kind: model.KindFile, Size: &size, Etag: &etag}, nil
}
func (s *zipFixtureStorage) OpenPath(ctx context.Context, path string, offset int64, length *int64) (DownloadStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if length == nil || offset < 0 || *length <= 0 || offset > int64(len(s.body)) || *length > int64(len(s.body))-offset {
		return nil, errors.New("invalid fixture range")
	}
	s.ranges = append(s.ranges, [2]int64{offset, *length})
	if s.changeOnOpen {
		s.changed = true
	}
	var reader io.ReadCloser = io.NopCloser(bytes.NewReader(s.body[offset : offset+*length]))
	if s.block != nil {
		reader = s.block
		close(s.entered)
	}
	status := s.status
	if status == 0 {
		status = 206
	}
	return &zipFixtureStream{reader, offset, *length, int64(len(s.body)), status}, nil
}

func zipBrowseHarness(t *testing.T, body []byte) (*RESTDispatcher, *zipFixtureStorage) {
	t.Helper()
	store := &zipFixtureStorage{body: body}
	return newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, store), store
}
func zipRequest(t *testing.T, d *RESTDispatcher, suffix, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	readRouter(t, d).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/zip/"+suffix+"?path=/fixture.zip"+query, nil))
	return w
}

func TestZIPBrowseHierarchiesAndSingleDownloads(t *testing.T) {
	body := zipFixture(t, zipFixtureFile{"docs/", nil, zip.Store, fs.ModeDir | 0755}, zipFixtureFile{"docs/中文 %2F.txt", []byte("content with 中文"), zip.Deflate, 0}, zipFixtureFile{"implicit/sub/note.txt", []byte("nested"), zip.Store, 0}, zipFixtureFile{"empty.txt", nil, zip.Store, 0})
	d, _ := zipBrowseHarness(t, body)
	w := zipRequest(t, d, "entries", "")
	var response struct {
		Entry   string      `json:"entry"`
		Entries []zipMember `json:"entries"`
		Total   int         `json:"total_entries"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Entry != "/" || response.Total != 4 || len(response.Entries) != 3 {
		t.Fatalf("root listing: %d %s", w.Code, w.Body.String())
	}
	if response.Entries[0].Path != "/docs" || response.Entries[0].Kind != "folder" || response.Entries[1].Path != "/implicit" {
		t.Fatalf("hierarchy: %+v", response.Entries)
	}
	w = zipRequest(t, d, "entries", "&entry=/docs")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "%2F.txt") {
		t.Fatalf("nested listing: %d %s", w.Code, w.Body.String())
	}
	w = zipRequest(t, d, "download", "&entry=/docs/%E4%B8%AD%E6%96%87%20%252F.txt")
	if w.Code != 200 || w.Body.String() != "content with 中文" || !strings.Contains(w.Header().Get("Content-Disposition"), "%252F.txt") || w.Header().Get("Content-Length") != "" {
		t.Fatalf("download: %d %s %v", w.Code, w.Body.String(), w.Header())
	}
	w = zipRequest(t, d, "download", "&entry=/empty.txt")
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("empty member download failed")
	}
	for _, query := range []string{"&entry=../outside", "&entry=/../outside", "&entry=/a//b", "&entry=/a&entry=/b"} {
		if w := zipRequest(t, d, "entries", query); w.Code != 400 {
			t.Fatalf("unsafe selection accepted: %s %d", query, w.Code)
		}
	}
	if w := zipRequest(t, d, "entries", "&entry=/missing"); w.Code != 404 {
		t.Fatal("missing member not found contract")
	}
	if w := zipRequest(t, d, "download", "&entry=/docs"); w.Code != 409 {
		t.Fatal("directory download allowed")
	}
}

func TestZIPBrowseListsUsingOnlyTailAndCentralDirectoryRanges(t *testing.T) {
	contents := make([]byte, 900<<10)
	for i := range contents {
		contents[i] = byte(i % 251)
	}
	body := zipFixture(t, zipFixtureFile{"large.bin", contents, zip.Store, 0})
	d, store := zipBrowseHarness(t, body)
	if w := zipRequest(t, d, "entries", ""); w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var read int64
	for _, span := range store.ranges {
		read += span[1]
	}
	if read > 100<<10 || len(store.ranges) > 3 {
		t.Fatalf("listing read file contents: %v, %d bytes", store.ranges, read)
	}
	store.ranges = nil
	w := zipRequest(t, d, "download", "&entry=/large.bin")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), contents) {
		t.Fatal("large streamed member bytes differ")
	}
	if len(store.ranges) > 8 {
		t.Fatalf("small inflate reads caused excessive remote calls: %d", len(store.ranges))
	}
}

func TestZIPBrowseRejectsUnsafeMembersBeforeOpening(t *testing.T) {
	for _, test := range []struct {
		name  string
		files []zipFixtureFile
		want  int
	}{
		{"traversal", []zipFixtureFile{{"../escape", []byte("x"), zip.Store, 0}}, 400},
		{"absolute", []zipFixtureFile{{"/escape", []byte("x"), zip.Store, 0}}, 400},
		{"backslash", []zipFixtureFile{{`a\b`, []byte("x"), zip.Store, 0}}, 400},
		{"drive", []zipFixtureFile{{"C:escape", []byte("x"), zip.Store, 0}}, 400},
		{"bad folder", []zipFixtureFile{{"folder//", nil, zip.Store, 0}}, 400},
		{"symlink", []zipFixtureFile{{"link", []byte("target"), zip.Store, fs.ModeSymlink | 0777}}, 501},
		{"case collision", []zipFixtureFile{{"A.txt", nil, zip.Store, 0}, {"a.txt", nil, zip.Store, 0}}, 409},
		{"duplicate", []zipFixtureFile{{"a.txt", nil, zip.Store, 0}, {"a.txt", nil, zip.Store, 0}}, 409},
		{"file folder collision", []zipFixtureFile{{"folder", nil, zip.Store, 0}, {"folder/file", nil, zip.Store, 0}}, 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, _ := zipBrowseHarness(t, zipFixture(t, test.files...))
			w := zipRequest(t, d, "entries", "")
			if w.Code != test.want {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func([]byte)
		want   int
	}{
		{"encrypted", func(h []byte) { binary.LittleEndian.PutUint16(h[8:], 1) }, 501},
		{"unsupported codec", func(h []byte) { binary.LittleEndian.PutUint16(h[10:], 99) }, 501},
		{"Mac symlink", func(h []byte) { h[5] = 19; binary.LittleEndian.PutUint32(h[38:], 0120777<<16) }, 501},
		{"multi disk", func(h []byte) { binary.LittleEndian.PutUint16(h[34:], 1) }, 501},
		{"offset beyond file", func(h []byte) { binary.LittleEndian.PutUint32(h[42:], 0xfffffffe) }, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := zipFixture(t, zipFixtureFile{"safe", []byte("x"), zip.Store, 0})
			offset := bytes.Index(body, []byte{'P', 'K', 1, 2})
			test.mutate(body[offset:])
			d, _ := zipBrowseHarness(t, body)
			w := zipRequest(t, d, "entries", "")
			if w.Code != test.want {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}

func zip64Fixture(t *testing.T, count uint64) []byte {
	body := zipFixture(t, zipFixtureFile{"safe.txt", []byte("safe"), zip.Store, 0})
	end := bytes.LastIndex(body, []byte{'P', 'K', 5, 6})
	directorySize := binary.LittleEndian.Uint32(body[end+12:])
	directoryOffset := binary.LittleEndian.Uint32(body[end+16:])
	record := make([]byte, 56)
	binary.LittleEndian.PutUint32(record, 0x06064b50)
	binary.LittleEndian.PutUint64(record[4:], 44)
	binary.LittleEndian.PutUint64(record[24:], count)
	binary.LittleEndian.PutUint64(record[32:], count)
	binary.LittleEndian.PutUint64(record[40:], uint64(directorySize))
	binary.LittleEndian.PutUint64(record[48:], uint64(directoryOffset))
	locator := make([]byte, 20)
	binary.LittleEndian.PutUint32(locator, 0x07064b50)
	binary.LittleEndian.PutUint64(locator[8:], uint64(end))
	binary.LittleEndian.PutUint32(locator[16:], 1)
	eocd := append([]byte(nil), body[end:]...)
	binary.LittleEndian.PutUint16(eocd[8:], 65535)
	binary.LittleEndian.PutUint16(eocd[10:], 65535)
	binary.LittleEndian.PutUint32(eocd[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(eocd[16:], 0xffffffff)
	return append(append(append(body[:end], record...), locator...), eocd...)
}

func TestZIPBrowseZIP64AndPreallocationLimits(t *testing.T) {
	d, _ := zipBrowseHarness(t, zip64Fixture(t, 1))
	if w := zipRequest(t, d, "download", "&entry=/safe.txt"); w.Code != 200 || w.Body.String() != "safe" {
		t.Fatalf("valid ZIP64: %d %s", w.Code, w.Body.String())
	}
	d, store := zipBrowseHarness(t, zip64Fixture(t, 1<<48))
	if w := zipRequest(t, d, "entries", ""); w.Code != 507 || len(store.ranges) != 1 {
		t.Fatalf("ZIP64 count not guarded before central allocation: %d %d", w.Code, len(store.ranges))
	}
	body := zipFixture(t, zipFixtureFile{"x", nil, zip.Store, 0})
	end := bytes.LastIndex(body, []byte{'P', 'K', 5, 6})
	binary.LittleEndian.PutUint32(body[end+12:], zipBrowseDirectoryLimit+1)
	d, store = zipBrowseHarness(t, body)
	if w := zipRequest(t, d, "entries", ""); w.Code != 507 || len(store.ranges) != 1 {
		t.Fatalf("central bytes guard: %d %s", w.Code, w.Body.String())
	}
	d, store = zipBrowseHarness(t, zipFixture(t))
	store.metaSize = model.Ptr(zipBrowseArchiveLimit + 1)
	if w := zipRequest(t, d, "entries", ""); w.Code != 507 || len(store.ranges) != 0 {
		t.Fatal("oversize archive opened a range")
	}
}

func TestZIPBrowseNameBudgetPrecedesStandardLibraryDirectoryParsing(t *testing.T) {
	files := make([]zipFixtureFile, 1100)
	for i := range files {
		files[i] = zipFixtureFile{fmt.Sprintf("%04d-", i) + strings.Repeat("x", 3995), nil, zip.Store, 0}
	}
	d, store := zipBrowseHarness(t, zipFixture(t, files...))
	w := zipRequest(t, d, "entries", "")
	if w.Code != 507 || len(store.ranges) != 2 {
		t.Fatalf("aggregate filename budget not enforced in preflight: %d %d", w.Code, len(store.ranges))
	}
}

func TestZIPBrowseExtractLimitsAndCRC(t *testing.T) {
	body := zipFixture(t, zipFixtureFile{"bomb.txt", bytes.Repeat([]byte("a"), 1<<20), zip.Deflate, 0})
	d, _ := zipBrowseHarness(t, body)
	if w := zipRequest(t, d, "entries", ""); w.Code != 200 {
		t.Fatal("size metadata should remain browsable")
	}
	if w := zipRequest(t, d, "download", "&entry=/bomb.txt"); w.Code != 507 {
		t.Fatalf("compression ratio not checked: %d", w.Code)
	}
	body = zipFixture(t, zipFixtureFile{"safe.txt", []byte("original bytes"), zip.Store, 0})
	body[30+len("safe.txt")] ^= 0xff
	d, _ = zipBrowseHarness(t, body)
	if w := zipRequest(t, d, "download", "&entry=/safe.txt"); w.Code != 400 {
		t.Fatalf("CRC failure should reject before response: %d %s", w.Code, w.Body.String())
	}
	zeroCRC := zipFixture(t, zipFixtureFile{"zero.txt", []byte("not-zero-crc"), zip.Store, 0})
	central := bytes.Index(zeroCRC, []byte{'P', 'K', 1, 2})
	binary.LittleEndian.PutUint16(zeroCRC[6:], 0)
	binary.LittleEndian.PutUint16(zeroCRC[central+8:], 0)
	binary.LittleEndian.PutUint32(zeroCRC[central+16:], 0)
	d, _ = zipBrowseHarness(t, zeroCRC)
	if w := zipRequest(t, d, "download", "&entry=/zero.txt"); w.Code != 400 {
		t.Fatalf("zero checksum bypassed validation: %d", w.Code)
	}
	large := zipFixture(t, zipFixtureFile{"large", bytes.Repeat([]byte("x"), 300<<10), zip.Store, 0})
	large[30+len("large")+(290<<10)] ^= 0xff
	d, _ = zipBrowseHarness(t, large)
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("partial CRC failure did not abort: %v", recovered)
			}
		}()
		zipRequest(t, d, "download", "&entry=/large")
		t.Fatal("corrupt stream looked complete")
	}()
	exact := zipFixture(t, zipFixtureFile{"exact", bytes.Repeat([]byte("x"), 128<<10), zip.Store, 0})
	central = bytes.Index(exact, []byte{'P', 'K', 1, 2})
	binary.LittleEndian.PutUint16(exact[6:], 0)
	binary.LittleEndian.PutUint16(exact[central+8:], 0)
	binary.LittleEndian.PutUint32(exact[central+16:], 0)
	d, _ = zipBrowseHarness(t, exact)
	recorder := httptest.NewRecorder()
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("zero CRC stream did not abort: %v", recovered)
			}
		}()
		readRouter(t, d).ServeHTTP(recorder, httptest.NewRequest("GET", "/api/v1/zip/download?path=/fixture.zip&entry=/exact", nil))
		t.Fatal("zero CRC stream completed")
	}()
	if recorder.Body.Len() >= 128<<10 {
		t.Fatal("final output chunk escaped before CRC validation")
	}
}

func TestZIPBrowseLimitsInflateSizesAndConcurrentPermits(t *testing.T) {
	body := zipFixture(t, zipFixtureFile{"large", []byte("x"), zip.Store, 0})
	central := bytes.Index(body, []byte{'P', 'K', 1, 2})
	// Uncompressed size remains useful listing metadata but cannot be
	// used to allocate an oversized selected member.
	binary.LittleEndian.PutUint32(body[central+24:], zipBrowseOutputLimit+1)
	d, _ := zipBrowseHarness(t, body)
	if w := zipRequest(t, d, "download", "&entry=/large"); w.Code != 507 {
		t.Fatalf("inflated size guard: %d %s", w.Code, w.Body.String())
	}
	first, err := zipBrowsePermit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := zipBrowsePermit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := zipBrowsePermit(ctx); !errors.Is(err, context.Canceled) || release != nil {
		t.Fatalf("cancelled waiter: %v", err)
	}
	if len(zipBrowseWaiting) != 0 {
		t.Fatal("cancelled waiter leaked its queue slot")
	}
}

func TestZIPBrowseRangeIdentityCancellationAndBudgets(t *testing.T) {
	body := zipFixture(t, zipFixtureFile{"a.txt", []byte("a"), zip.Store, 0})
	for _, wrongRange := range []bool{false, true} {
		d, store := zipBrowseHarness(t, body)
		if wrongRange {
			store.status = 200
		} else {
			store.changeOnOpen = true
		}
		w := zipRequest(t, d, "entries", "")
		want := 409
		if wrongRange {
			want = 502
		}
		if w.Code != want {
			t.Fatalf("range/identity change: %d %s", w.Code, w.Body.String())
		}
	}
	d, store := zipBrowseHarness(t, body)
	reader, writer := io.Pipe()
	defer writer.Close()
	store.block, store.entered = reader, make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("GET", "/api/v1/zip/entries?path=/fixture.zip", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { readRouter(t, d).ServeHTTP(httptest.NewRecorder(), request); close(done) }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("range did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not close remote ZIP range")
	}
	d, store = zipBrowseHarness(t, body)
	entry, _ := store.Metadata("")
	remote := &zipRangeReader{d: d, ctx: context.Background(), entry: entry, size: int64(len(body)), requests: zipBrowseRequestLimit}
	if _, err := remote.remote(0, 1); err == nil || len(store.ranges) != 0 {
		t.Fatal("request budget not enforced")
	}
	remote.requests = 0
	remote.readBytes = zipBrowseReadLimit
	if _, err := remote.remote(0, 1); err == nil {
		t.Fatal("byte budget not enforced")
	}
}
