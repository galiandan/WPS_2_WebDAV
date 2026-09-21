package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

type searchTree struct {
	mu            sync.Mutex
	entries       map[string][]model.RemoteEntry
	fail          map[string]bool
	calls         []string
	block         chan struct{}
	entered       chan struct{}
	invalidations atomic.Int32
}

func (s *searchTree) InvalidateMetadataCache() { s.invalidations.Add(1) }

func (s *searchTree) Metadata(path string) (model.RemoteEntry, error) {
	if path == "/" {
		return model.RemoteEntry{ID: "root", Kind: model.KindFolder}, nil
	}
	for parent, entries := range s.entries {
		for _, entry := range entries {
			if strings.TrimSuffix(parent, "/")+"/"+entry.Name == path {
				return entry, nil
			}
		}
	}
	return model.RemoteEntry{}, errors.New("not found")
}

func (s *searchTree) ListPath(path string) ([]model.RemoteEntry, error) {
	s.mu.Lock()
	s.calls = append(s.calls, path)
	s.mu.Unlock()
	if s.block != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-s.block
	}
	if s.fail[path] {
		return nil, errors.New("private upstream details must not be returned")
	}
	return s.entries[path], nil
}

func (s *searchTree) ListChildren(path string, entry model.RemoteEntry) ([]model.RemoteEntry, error) {
	return s.ListPath(path)
}

func searchFolder(id, name string) model.RemoteEntry {
	return model.RemoteEntry{ID: id, Name: name, Kind: model.KindFolder}
}

func searchFixture() *searchTree {
	return &searchTree{entries: map[string][]model.RemoteEntry{
		"/":      {searchFolder("space:a", "工作"), searchFolder("space:b", "家庭")},
		"/工作":    {searchFolder("docs", "报告"), fileEntry("f1", "年度预算.XLSX")},
		"/家庭":    {searchFolder("docs", "照片")}, // IDs can repeat across spaces.
		"/工作/报告": {fileEntry("f2", "Q3报告.pdf"), fileEntry("f3", "README.md")},
		"/家庭/照片": {fileEntry("f4", "合照.JPG"), fileEntry("f5", "视频.mp4"), fileEntry("f6", "音乐.mp3")},
	}}
}

func testSearchIndex(t *testing.T, tree *searchTree, adjust func(*SearchLimits)) *SearchIndex {
	t.Helper()
	limits := defaultSearchLimits()
	limits.Interval = 0
	if adjust != nil {
		adjust(&limits)
	}
	index := newSearchIndex(tree, limits)
	t.Cleanup(func() { index.stop() })
	return index
}

func waitSearch(t *testing.T, index *SearchIndex) SearchIndexStatus {
	t.Helper()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		index.mu.Lock()
		active, status := index.active, index.status
		index.mu.Unlock()
		if !active {
			return status
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("search worker did not finish")
		}
	}
}

func TestSearchIndexRecursiveScopesTypesAndPagination(t *testing.T) {
	index := testSearchIndex(t, searchFixture(), nil)
	if _, err := index.start("/"); err != nil {
		t.Fatal(err)
	}
	status := waitSearch(t, index)
	if status.State != "ready" || !status.Complete || status.Entries != 10 || status.ScannedFolders != 5 || status.UpdatedAt == "" {
		t.Fatalf("unexpected complete snapshot: %+v", status)
	}
	tests := []struct {
		query url.Values
		want  []string
	}{
		{url.Values{"q": {"报告"}}, []string{"/工作/报告", "/工作/报告/Q3报告.pdf"}},
		{url.Values{"q": {"readme"}}, []string{"/工作/报告/README.md"}},
		{url.Values{"q": {"报告"}, "match": {"path"}, "type": {"file"}}, []string{"/工作/报告/Q3报告.pdf", "/工作/报告/README.md"}},
		{url.Values{"type": {"image"}}, []string{"/家庭/照片/合照.JPG"}},
		{url.Values{"type": {"audio"}}, []string{"/家庭/照片/音乐.mp3"}},
		{url.Values{"type": {"video"}}, []string{"/家庭/照片/视频.mp4"}},
		{url.Values{"type": {"document"}, "q": {"预算"}}, []string{"/工作/年度预算.XLSX"}},
		{url.Values{"path": {"/工作/报告/"}, "q": {"read"}}, []string{"/工作/报告/README.md"}},
		{url.Values{"path": {"/工作"}, "type": {"image"}}, nil},
		{url.Values{"path": {"/工"}}, nil},
	}
	for _, test := range tests {
		query, err := parseSearchQuery(test.query)
		if err != nil {
			t.Fatal(err)
		}
		response := index.query(query)
		if !response.ScopeCovered || response.Total != len(test.want) {
			t.Fatalf("query %v: %+v", test.query, response)
		}
		for n, expected := range test.want {
			if response.Results[n].Path != expected {
				t.Errorf("query %v: path %q, want %q", test.query, response.Results[n].Path, expected)
			}
		}
	}
	query, _ := parseSearchQuery(url.Values{"type": {"file"}, "offset": {"2"}, "limit": {"2"}})
	response := index.query(query)
	if response.Total != 6 || len(response.Results) != 2 || !response.HasMore {
		t.Fatalf("pagination = %+v", response)
	}
	query.offset = 6
	response = index.query(query)
	if len(response.Results) != 0 || response.HasMore {
		t.Fatalf("empty last page = %+v", response)
	}
}

func TestSearchIndexScopedRefreshAndCoverage(t *testing.T) {
	index := testSearchIndex(t, searchFixture(), nil)
	if _, err := index.start("/工作"); err != nil {
		t.Fatal(err)
	}
	waitSearch(t, index)
	query, _ := parseSearchQuery(url.Values{"path": {"/工作"}, "type": {"file"}})
	if got := index.query(query); !got.ScopeCovered || got.Total != 3 {
		t.Fatalf("scoped search = %+v", got)
	}
	query.path = "/"
	if got := index.query(query); got.ScopeCovered || got.Total != 0 {
		t.Fatalf("partial scope presented as global = %+v", got)
	}
}

func TestSearchIndexLimitsAndPartialFailures(t *testing.T) {
	tests := []struct {
		name   string
		adjust func(*SearchLimits)
		mutate func(*searchTree)
		reason string
	}{
		{"entries", func(l *SearchLimits) { l.MaxEntries = 3 }, nil, "entry_limit"},
		{"folders", func(l *SearchLimits) { l.MaxFolders = 2 }, nil, "folder_limit"},
		{"depth", func(l *SearchLimits) { l.MaxDepth = 1 }, nil, "depth_limit"},
		{"bytes", func(l *SearchLimits) { l.MaxBytes = 1 }, nil, "metadata_limit"},
		{"listing", nil, func(s *searchTree) { s.fail = map[string]bool{"/工作": true} }, "listing_failed"},
		{"cycle", nil, func(s *searchTree) { s.entries["/工作/报告"] = []model.RemoteEntry{searchFolder("docs", "cycle")} }, "repeated_folder"},
		{"ambiguous", nil, func(s *searchTree) {
			s.entries["/家庭/照片"] = []model.RemoteEntry{fileEntry("a", "x"), fileEntry("b", "x")}
		}, "ambiguous_path"},
		{"unsafe path", nil, func(s *searchTree) {
			s.entries["/家庭/照片"] = []model.RemoteEntry{searchFolder("unsafe", "../other")}
		}, "invalid_entry"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree := searchFixture()
			if test.mutate != nil {
				test.mutate(tree)
			}
			index := testSearchIndex(t, tree, test.adjust)
			_, _ = index.start("/")
			status := waitSearch(t, index)
			if status.Complete || status.State != "partial" || status.Reason != test.reason {
				t.Fatalf("status = %+v", status)
			}
			if test.name == "listing" {
				query, _ := parseSearchQuery(url.Values{"type": {"image"}})
				if index.query(query).Total != 1 || status.SkippedFolders != 1 {
					t.Fatal("one failed space prevented another space from being indexed")
				}
			}
		})
	}
}

func TestSearchIndexCancelDoesNotOverlapListingOrRetainInvalidatedData(t *testing.T) {
	tree := searchFixture()
	tree.block, tree.entered = make(chan struct{}), make(chan struct{}, 1)
	index := testSearchIndex(t, tree, nil)
	_, _ = index.start("/")
	<-tree.entered
	if got := index.stop(); got.State != "cancelling" {
		t.Fatalf("cancel status = %+v", got)
	}
	if _, err := index.start("/"); !errors.Is(err, errSearchBusy) {
		t.Fatal("allowed overlapping worker while a listing was still in flight")
	}
	index.invalidate()
	close(tree.block)
	if status := waitSearch(t, index); status.State != "idle" || status.Entries != 0 {
		t.Fatalf("old worker republished invalidated data: %+v", status)
	}
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if len(tree.calls) != 1 {
		t.Fatalf("cancelled traversal still listed more folders: %v", tree.calls)
	}
}

func TestSearchIndexCancellationAndDeadlineAreIncomplete(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		tree := searchFixture()
		tree.block, tree.entered = make(chan struct{}), make(chan struct{}, 1)
		index := testSearchIndex(t, tree, func(l *SearchLimits) {
			if deadline {
				l.MaxDuration = time.Millisecond
			}
		})
		_, _ = index.start("/")
		if deadline {
			time.Sleep(5 * time.Millisecond)
		} else {
			<-tree.entered
			index.stop()
		}
		close(tree.block)
		status := waitSearch(t, index)
		want := "cancelled"
		if deadline {
			want = "time_limit"
		}
		if status.Complete || status.Reason != want || status.Entries != 0 {
			t.Fatalf("deadline %t: %+v", deadline, status)
		}
	}
}

func TestSearchIdentityHotReloadInvalidatesSnapshotAndInFlightBuild(t *testing.T) {
	var identity atomic.Uint32
	var unavailable atomic.Bool
	tree := searchFixture()
	index := testSearchIndex(t, tree, nil)
	index.identitySource = func() ([32]byte, error) {
		if unavailable.Load() {
			return [32]byte{}, errors.New("secret details")
		}
		return [32]byte{byte(identity.Load())}, nil
	}
	_, _ = index.start("/")
	waitSearch(t, index)
	if tree.invalidations.Load() != 1 {
		t.Fatal("explicit refresh reused the directory cache")
	}
	identity.Add(1)
	if err := index.refreshIdentity(); err != nil {
		t.Fatal(err)
	}
	if tree.invalidations.Load() != 2 {
		t.Fatal("credential change retained cached directory listings")
	}
	query, _ := parseSearchQuery(url.Values{})
	if got := index.query(query); got.Total != 0 || got.Index.State != "idle" {
		t.Fatalf("old identity still searchable: %+v", got)
	}
	tree.block, tree.entered = make(chan struct{}), make(chan struct{}, 1)
	_, _ = index.start("/")
	<-tree.entered
	identity.Add(1)
	close(tree.block)
	if got := waitSearch(t, index); got.State != "idle" || got.Entries != 0 {
		t.Fatalf("identity changed during listing: %+v", got)
	}
	unavailable.Store(true)
	if err := index.refreshIdentity(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("identity error must be redacted: %v", err)
	}
}

func TestRESTSearchRoutesAndValidation(t *testing.T) {
	tree := searchFixture()
	tree.entries["/工作/报告"][1].LinkID = model.Ptr("private-link-id")
	dispatcher := newReadDispatcher(t, tree, nil)
	dispatcher.searchIndex().limits.Interval = 0
	t.Cleanup(dispatcher.CancelSearch)
	router := readRouter(t, dispatcher)
	call := func(method, target string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, newTestRequest(method, target))
		return recorder
	}
	if got := call("GET", "/api/v1/search"); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"state":"idle"`) {
		t.Fatalf("idle query: %d %s", got.Code, got.Body.String())
	}
	if got := call("POST", "/api/v1/search/refresh?path=/"); got.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", got.Code, got.Body.String())
	}
	waitSearch(t, dispatcher.searchIndex())
	got := call("GET", "/api/v1/search?q=README&type=document")
	var response searchResponse
	if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &response) != nil || response.Total != 1 {
		t.Fatalf("query: %d %s", got.Code, got.Body.String())
	}
	if strings.Contains(got.Body.String(), "private-link-id") || strings.Contains(got.Body.String(), "identity") {
		t.Fatal("internal metadata leaked")
	}
	for _, query := range []string{"q=a&q=b", "path=/%2E%2E", "path=", "type=executable", "match=body", "offset=-1", "limit=0", "limit=201", "q=%00", "limit=2&limit=3", "q=" + strings.Repeat("a", 257)} {
		if got := call("GET", "/api/v1/search?"+query); got.Code != http.StatusBadRequest {
			t.Errorf("invalid query %q accepted: %d %s", query, got.Code, got.Body.String())
		}
	}
	if got := call("DELETE", "/api/v1/search"); got.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", got.Code, got.Body.String())
	}
}
